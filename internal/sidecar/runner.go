package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
	httpClient       *http.Client
	maxRetries       int
}

// NewRunner creates a new sidecar runner.
func NewRunner(cfg *Config) (*Runner, error) {
	var events []string
	if cfg.CallbackEvents != "" {
		events = strings.Split(cfg.CallbackEvents, ",")
	}

	var artifacts []artifact.Artifact
	if cfg.ArtifactsJSON != "" && cfg.ArtifactsJSON != "[]" {
		var err error
		artifacts, err = artifact.UnmarshalArtifacts([]byte(cfg.ArtifactsJSON))
		if err != nil {
			return nil, fmt.Errorf("failed to parse artifacts: %w", err)
		}
	}

	// Separate artifacts into pre-job and post-job based on dependencies
	preJob, postJob := separateArtifacts(artifacts)

	var meta map[string]string
	if cfg.Meta != "" {
		_ = json.Unmarshal([]byte(cfg.Meta), &meta)
	}

	maxRetries := cfg.UploadRetries
	if maxRetries < 0 {
		maxRetries = 3
	}

	return &Runner{
		config:           cfg,
		events:           events,
		preJobArtifacts:  preJob,
		postJobArtifacts: postJob,
		sender:           cloudevent.NewSender(cfg.CallbackTimeout),
		eventBuilder:     job.NewEventBuilder(cfg.JobID, "orchestrator/sidecar", meta),
		httpClient:       &http.Client{Timeout: cfg.UploadTimeout},
		maxRetries:       maxRetries,
	}, nil
}

// separateArtifacts splits artifacts into pre-job and post-job based on dependencies.
// An artifact runs post-job if it depends on "job" or transitively depends on something that does.
func separateArtifacts(artifacts []artifact.Artifact) (preJob, postJob []artifact.Artifact) {
	// Build dependency graph
	dependsOnJob := make(map[string]bool)

	// First pass: mark artifacts that directly depend on "job"
	for _, a := range artifacts {
		if a.DependsOn() == artifact.JobDependency {
			dependsOnJob[a.ArtifactID()] = true
		}
	}

	// Iteratively mark artifacts that depend on post-job artifacts
	changed := true
	for changed {
		changed = false
		for _, a := range artifacts {
			if !dependsOnJob[a.ArtifactID()] && dependsOnJob[a.DependsOn()] {
				dependsOnJob[a.ArtifactID()] = true
				changed = true
			}
		}
	}

	// Split based on the computed set
	for _, a := range artifacts {
		if dependsOnJob[a.ArtifactID()] {
			postJob = append(postJob, a)
		} else {
			preJob = append(preJob, a)
		}
	}

	return preJob, postJob
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
	if len(artifacts) == 0 {
		return nil
	}

	completed := make(map[string]bool)
	var lastErr error

	for len(completed) < len(artifacts) {
		progress := false
		for _, a := range artifacts {
			id := a.ArtifactID()
			if completed[id] {
				continue
			}

			// Check if dependencies are satisfied (skip "job" dependency for post-job)
			dep := a.DependsOn()
			if dep == artifact.JobDependency {
				dep = ""
			}
			if dep != "" && !completed[dep] {
				continue
			}

			// For post-job artifacts, wait for the source file to exist
			if waitForFiles {
				if srcPath := getArtifactSourcePath(a); srcPath != "" {
					fullPath := filepath.Join(r.config.SharedVolumePath, srcPath)
					if err := r.waitForPath(ctx, fullPath); err != nil {
						r.sendArtifactEvent(ctx, a, "failed", nil, err)
						slog.With("artifactId", id, "error", err).Warn("Artifact failed (file not found)")
						completed[id] = true
						progress = true
						continue
					}
				}
			}

			// Inject HTTP client for types that need it
			switch art := a.(type) {
			case *artifact.Download:
				art.SetHTTPClient(r.httpClient)
			case *artifact.Upload:
				art.SetHTTPClient(r.httpClient, r.maxRetries)
			}
			result := a.Apply(ctx, r.config.SharedVolumePath)
			if result.Error != nil {
				lastErr = result.Error
			}

			r.sendArtifactEvent(ctx, a, result.Status, result.Content, result.Error)

			logger := slog.With("artifactId", id, "type", a.ArtifactType(), "status", result.Status)
			if result.Error != nil {
				logger = logger.With("error", result.Error)
			}
			logger.Info("Artifact processed")

			completed[id] = true
			progress = true
		}

		if !progress {
			for _, a := range artifacts {
				if !completed[a.ArtifactID()] {
					slog.Warn("Artifact skipped due to unresolved dependency",
						"artifactId", a.ArtifactID(),
						"depends", a.DependsOn())
				}
			}
			break
		}
	}

	return lastErr
}

// getArtifactSourcePath returns the source path for artifacts that read from files.
func getArtifactSourcePath(a artifact.Artifact) string {
	switch art := a.(type) {
	case *artifact.Upload:
		return art.In
	case *artifact.Read:
		return art.In
	case *artifact.Archive:
		return art.In
	case *artifact.List:
		return art.In
	default:
		return ""
	}
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
