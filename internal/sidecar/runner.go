package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"orchestrator/pkg/cloudevent"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ReadyFile is the marker file written when pre-job artifacts are complete.
// Docker orchestrator uses this with health checks to know when to start the worker.
// Kubernetes uses startup probes on this file for native sidecar containers.
const ReadyFile = ".ready"

// Runner orchestrates the sidecar flow.
// The sidecar handles artifact processing (downloads, uploads, archives, etc.)
// with callbacks for each artifact.
//
// Log streaming, start, and exit events are handled by the supervisor.
type Runner struct {
	config           *Config
	events           []string // parsed from config
	preJobArtifacts  []artifact.Artifact
	postJobArtifacts []artifact.Artifact
	sender           *cloudevent.Sender
	eventBuilder     *job.EventBuilder
	registry         *artifact.Registry
}

// NewRunner creates a new sidecar runner.
func NewRunner(cfg *Config, reg *artifact.Registry) (*Runner, error) {
	var events []string
	if cfg.CallbackEvents != "" {
		events = strings.Split(cfg.CallbackEvents, ",")
	}

	var artifacts []artifact.Artifact
	if cfg.ArtifactsJSON != "" && cfg.ArtifactsJSON != "[]" {
		var err error
		artifacts, err = reg.Unmarshal([]byte(cfg.ArtifactsJSON))
		if err != nil {
			return nil, fmt.Errorf("failed to parse artifacts: %w", err)
		}
	}

	// Separate artifacts into pre-job and post-job based on dependencies
	preJob, postJob := artifact.Partition(artifacts)

	var meta map[string]string
	if cfg.Meta != "" {
		_ = json.Unmarshal([]byte(cfg.Meta), &meta)
	}

	return &Runner{
		config:           cfg,
		events:           events,
		preJobArtifacts:  preJob,
		postJobArtifacts: postJob,
		sender:           cloudevent.NewSender(cfg.CallbackTimeout),
		eventBuilder:     job.NewEventBuilder(cfg.JobID, "orchestrator/sidecar", meta),
		registry:         reg,
	}, nil
}

// Run executes the sidecar flow:
// 1. Process pre-job artifacts (downloads, file writes, etc.)
// 2. Wait for completion signal (SIGUSR1 from Docker, SIGTERM from Kubernetes)
// 3. Process post-job artifacts (uploads, events, etc.)
//
// If any pre-job artifact fails, the sidecar exits with an error.
func (r *Runner) Run(ctx context.Context) error {
	logger := slog.With("jobId", r.config.JobID, "preJob", len(r.preJobArtifacts), "postJob", len(r.postJobArtifacts))
	logger.Info("Sidecar starting")

	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.config.TimeoutSeconds)*time.Second)
	defer cancel()

	if err := r.processArtifacts(ctx, r.preJobArtifacts, false); err != nil {
		logger.Error("Pre-job artifact processing failed, aborting job", "error", err)
		return fmt.Errorf("pre-job artifact processing failed: %w", err)
	}

	// Write marker file to signal pre-job artifacts are ready
	markerPath := filepath.Join(r.config.SharedVolumePath, ReadyFile)
	if err := os.WriteFile(markerPath, []byte{}, 0o644); err != nil {
		logger.Error("Failed to write ready marker", "error", err)
		return fmt.Errorf("failed to write ready marker: %w", err)
	}
	logger.Info("Pre-job artifacts ready", "path", markerPath)

	// Wait for worker completion signal
	logger.Info("Waiting for worker completion signal")
	r.waitForSignal(ctx)
	logger.Info("Received worker completion signal")

	// Process post-job artifacts (uploads, reads, etc.)
	_ = r.processArtifacts(ctx, r.postJobArtifacts, true)

	logger.Info("Sidecar completed")
	return nil
}

// waitForSignal blocks until a completion signal is received or context is cancelled.
func (r *Runner) waitForSignal(ctx context.Context) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
	case <-sigCh:
	}
}

// processArtifacts processes artifacts in dependency order.
// For post-job artifacts, it waits for files to appear before processing.
func (r *Runner) processArtifacts(ctx context.Context, artifacts []artifact.Artifact, waitForFiles bool) error {
	return artifact.RunInOrder(ctx, artifacts, func(ctx context.Context, a artifact.Artifact) error {
		if waitForFiles {
			if srcPath := r.registry.SourcePath(a); srcPath != "" {
				fullPath := filepath.Join(r.config.SharedVolumePath, srcPath)
				if err := r.waitForPath(ctx, fullPath); err != nil {
					r.sendArtifactEvent(ctx, a, "failed", nil, err)
					slog.With("artifactId", a.ArtifactID(), "error", err).Warn("Artifact failed (file not found)")
					return err
				}
			}
		}

		result := a.Apply(ctx, r.config.SharedVolumePath)
		r.sendArtifactEvent(ctx, a, result.Status, result.Content, result.Error)

		logger := slog.With("artifactId", a.ArtifactID(), "type", a.ArtifactType(), "status", result.Status)
		if result.Error != nil {
			logger = logger.With("error", result.Error)
		}
		logger.Info("Artifact processed")

		return result.Error
	})
}

func (r *Runner) sendArtifactEvent(ctx context.Context, a artifact.Artifact, status string, content any, err error) {
	event := r.eventBuilder.BuildArtifactEvent(a.ArtifactID(), a.ArtifactType(), status, content, err)
	if sendErr := r.sendEvent(ctx, event); sendErr != nil {
		slog.With("artifactId", a.ArtifactID(), "callbackError", sendErr).Warn("Failed to send artifact event")
	}
}

func (r *Runner) sendEvent(ctx context.Context, event *cloudevent.CloudEvent) error {
	if r.config.CallbackURL == "" {
		return nil
	}
	if !job.FilteredEvents(event.Type, r.events) {
		return nil
	}

	opts := cloudevent.SendOptions{}
	if r.config.CallbackKey != "" {
		signature, err := cloudevent.Sign(event, r.config.CallbackKey)
		if err != nil {
			return fmt.Errorf("failed to sign event: %w", err)
		}
		opts.Signature = signature
	}

	return r.sender.Send(ctx, r.config.CallbackURL, event, opts)
}

func (r *Runner) waitForPath(ctx context.Context, path string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
		}
	}
}

// Close releases resources.
func (r *Runner) Close() error {
	return nil
}

// CheckReady checks if the ready marker file exists.
// Used by Docker health checks to determine when worker can start.
func CheckReady(sharedVolumePath string) bool {
	markerPath := filepath.Join(sharedVolumePath, ReadyFile)
	_, err := os.Stat(markerPath)
	return err == nil
}
