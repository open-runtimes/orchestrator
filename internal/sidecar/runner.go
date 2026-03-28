package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
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

// SignalFunc blocks until the worker has finished, then returns.
// ctx is passed so the implementation can respect cancellation/timeout.
// The default implementation waits for SIGUSR1 or SIGTERM from the worker process.
type SignalFunc func(ctx context.Context)

// ArtifactReporterFunc reports an artifact result to the orchestrator.
// The default implementation POSTs a job.ArtifactReport to the orchestrator's internal endpoint.
type ArtifactReporterFunc func(ctx context.Context, report job.ArtifactReport) error

// Option configures a Runner. Applied after production defaults are set in NewRunner.
type Option func(*Runner)

// WithSignalFunc replaces the default OS signal handler with fn.
// Used in tests to inject a channel-based trigger instead of SIGUSR1/SIGTERM.
func WithSignalFunc(fn SignalFunc) Option {
	return func(r *Runner) { r.waitFn = fn }
}

// WithArtifactReporter replaces the default HTTP reporter with fn.
// Used in tests to capture artifact reports without making real HTTP calls.
func WithArtifactReporter(fn ArtifactReporterFunc) Option {
	return func(r *Runner) { r.reportFn = fn }
}

// Runner orchestrates the sidecar flow.
// The sidecar handles artifact processing (downloads, uploads, archives, etc.)
// and reports results to the orchestrator, which dispatches the corresponding events.
//
// Log streaming, start, and exit events are handled by the supervisor.
type Runner struct {
	config           *Config
	meta             map[string]string
	preJobArtifacts  []artifact.Artifact
	postJobArtifacts []artifact.Artifact
	registry         *artifact.Registry
	waitFn           SignalFunc
	reportFn         ArtifactReporterFunc
}

// NewRunner creates a new sidecar runner. Production callers pass no options.
// Tests pass WithSignalFunc and/or WithArtifactReporter to replace OS-level seams.
func NewRunner(cfg *Config, reg *artifact.Registry, opts ...Option) (*Runner, error) {
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
	if cfg.Meta != "" && cfg.Meta != "{}" {
		_ = json.Unmarshal([]byte(cfg.Meta), &meta)
	}

	httpClient := &http.Client{Timeout: cfg.ArtifactTimeout}

	r := &Runner{
		config:           cfg,
		meta:             meta,
		preJobArtifacts:  preJob,
		postJobArtifacts: postJob,
		registry:         reg,
		waitFn: func(ctx context.Context) {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGTERM)
			defer signal.Stop(sigCh)
			select {
			case <-ctx.Done():
			case <-sigCh:
			}
		},
		reportFn: func(ctx context.Context, report job.ArtifactReport) error {
			if cfg.ArtifactEndpoint == "" {
				return nil
			}
			data, err := json.Marshal(report)
			if err != nil {
				return fmt.Errorf("failed to marshal report: %w", err)
			}
			url := fmt.Sprintf("%s/internal/jobs/%s/artifact", cfg.ArtifactEndpoint, cfg.JobID)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("artifact report failed: HTTP %d", resp.StatusCode)
			}
			return nil
		},
	}

	for _, o := range opts {
		o(r)
	}
	return r, nil
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
	r.waitFn(ctx)
	logger.Info("Received worker completion signal")

	// Process post-job artifacts (uploads, reads, etc.)
	_ = r.processArtifacts(ctx, r.postJobArtifacts, true)

	logger.Info("Sidecar completed")
	return nil
}

// processArtifacts processes artifacts in dependency order.
// For post-job artifacts, it waits for files to appear before processing.
func (r *Runner) processArtifacts(ctx context.Context, artifacts []artifact.Artifact, waitForFiles bool) error {
	return artifact.RunInOrder(ctx, artifacts, func(ctx context.Context, a artifact.Artifact) error {
		if waitForFiles {
			if srcPath := r.registry.SourcePath(a); srcPath != "" {
				fullPath := filepath.Join(r.config.SharedVolumePath, srcPath)
				if err := r.waitForPath(ctx, fullPath); err != nil {
					r.reportArtifact(ctx, a, "failed", nil, err)
					slog.With("artifactId", a.ArtifactID(), "error", err).Warn("Artifact failed (file not found)")
					return err
				}
			}
		}

		result := a.Apply(ctx, r.config.SharedVolumePath)
		r.reportArtifact(ctx, a, result.Status, result.Content, result.Error)

		logger := slog.With("artifactId", a.ArtifactID(), "type", a.ArtifactType(), "status", result.Status)
		if result.Error != nil {
			logger = logger.With("error", result.Error)
		}
		logger.Info("Artifact processed")

		return result.Error
	})
}

func (r *Runner) reportArtifact(ctx context.Context, a artifact.Artifact, status string, content any, err error) {
	report := job.ArtifactReport{
		JobID:        r.config.JobID,
		ID:   a.ArtifactID(),
		Type: a.ArtifactType(),
		Status:       status,
		Content:      content,
		CallbackURL:  r.config.CallbackURL,
		CallbackKey:  r.config.CallbackKey,
		Meta:         r.meta,
	}
	if r.config.CallbackEvents != "" {
		report.CallbackEvents = strings.Split(r.config.CallbackEvents, ",")
	}
	if err != nil {
		report.Error = err.Error()
	}
	if reportErr := r.reportFn(ctx, report); reportErr != nil {
		slog.With("artifactId", a.ArtifactID(), "error", reportErr).Warn("Failed to report artifact")
	}
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
