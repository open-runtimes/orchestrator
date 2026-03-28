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
	"orchestrator/pkg/emitter"
	"os"
	"os/signal"
	"path/filepath"
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

// Option configures a Runner. Applied after production defaults are set in NewRunner.
type Option func(*Runner)

// WithSignalFunc replaces the default OS signal handler with fn.
// Used in tests to inject a channel-based trigger instead of SIGUSR1/SIGTERM.
func WithSignalFunc(fn SignalFunc) Option {
	return func(r *Runner) { r.waitFn = fn }
}

// WithArtifactListener registers a listener that receives artifact reports.
// Multiple listeners can be registered; each receives every report.
func WithArtifactListener(fn func(job.ArtifactReport)) Option {
	return func(r *Runner) { r.emitter.Register(fn) }
}

// Runner orchestrates the sidecar flow.
// The sidecar handles artifact processing (downloads, uploads, archives, etc.)
// and reports results to the orchestrator, which dispatches the corresponding events.
//
// Log streaming, start, and exit events are handled by the supervisor.
type Runner struct {
	jobID            string
	sharedVolumePath string
	timeoutSeconds   int
	registry         *artifact.Registry
	waitFn           SignalFunc
	emitter          emitter.Emitter[job.ArtifactReport]
}

// NewRunner creates a new sidecar runner. Production callers pass WithArtifactListener.
// Tests pass WithSignalFunc and/or WithArtifactListener to replace OS-level seams.
func NewRunner(jobID, sharedVolumePath string, timeoutSeconds int, reg *artifact.Registry, opts ...Option) *Runner {
	r := &Runner{
		jobID:            jobID,
		sharedVolumePath: sharedVolumePath,
		timeoutSeconds:   timeoutSeconds,
		registry:         reg,
		waitFn:           waitForSignal,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// NewHTTPSink returns an emitter listener that POSTs artifact results to the orchestrator.
// It captures all job-level fields (callback config, meta) and merges them into each report.
// The HTTP client timeout governs per-request cancellation.
func NewHTTPSink(jobID, endpoint string, timeout time.Duration, callbackURL, callbackKey string, callbackEvents []string, meta map[string]string) func(job.ArtifactReport) {
	client := &http.Client{Timeout: timeout}
	return func(report job.ArtifactReport) {
		if endpoint == "" {
			return
		}
		report.JobID = jobID
		report.CallbackURL = callbackURL
		report.CallbackKey = callbackKey
		report.CallbackEvents = callbackEvents
		report.Meta = meta
		data, err := json.Marshal(report)
		if err != nil {
			slog.With("artifactId", report.ID, "error", err).Warn("Failed to marshal artifact report")
			return
		}
		url := fmt.Sprintf("%s/internal/jobs/%s/artifact", endpoint, jobID)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			slog.With("artifactId", report.ID, "error", err).Warn("Failed to build artifact report request")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			slog.With("artifactId", report.ID, "error", err).Warn("Failed to send artifact report")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			slog.With("artifactId", report.ID, "status", resp.StatusCode).Warn("Artifact report rejected")
		}
	}
}

// waitForSignal blocks until SIGUSR1 or SIGTERM is received, or ctx is done.
func waitForSignal(ctx context.Context) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	select {
	case <-ctx.Done():
	case <-sigCh:
	}
}

// Run executes the sidecar flow:
// 1. Process pre-job artifacts (downloads, file writes, etc.)
// 2. Wait for completion signal (SIGUSR1 from Docker, SIGTERM from Kubernetes)
// 3. Process post-job artifacts (uploads, events, etc.)
//
// If any pre-job artifact fails, the sidecar exits with an error.
func (r *Runner) Run(ctx context.Context, artifacts []artifact.Artifact) error {
	preJob, postJob := artifact.Partition(artifacts)

	logger := slog.With("jobId", r.jobID, "preJob", len(preJob), "postJob", len(postJob))
	logger.Info("Sidecar starting")

	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.timeoutSeconds)*time.Second)
	defer cancel()

	if err := r.processArtifacts(ctx, preJob, false); err != nil {
		logger.Error("Pre-job artifact processing failed, aborting job", "error", err)
		return fmt.Errorf("pre-job artifact processing failed: %w", err)
	}

	// Write marker file to signal pre-job artifacts are ready
	markerPath := filepath.Join(r.sharedVolumePath, ReadyFile)
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
	if err := r.processArtifacts(ctx, postJob, true); err != nil {
		logger.Warn("Post-job artifact processing failed", "error", err)
	}

	logger.Info("Sidecar completed")
	return nil
}

// processArtifacts processes artifacts in dependency order.
// For post-job artifacts, it waits for files to appear before processing.
func (r *Runner) processArtifacts(ctx context.Context, artifacts []artifact.Artifact, waitForFiles bool) error {
	return artifact.RunInOrder(ctx, artifacts, func(ctx context.Context, a artifact.Artifact) error {
		if waitForFiles {
			if srcPath := r.registry.SourcePath(a); srcPath != "" {
				fullPath := filepath.Join(r.sharedVolumePath, srcPath)
				if err := r.waitForPath(ctx, fullPath); err != nil {
					r.emitArtifact(a, "failed", nil, err)
					slog.With("artifactId", a.ArtifactID(), "error", err).Warn("Artifact failed (file not found)")
					return err
				}
			}
		}

		result := a.Apply(ctx, r.sharedVolumePath)
		r.emitArtifact(a, result.Status, result.Content, result.Error)

		logger := slog.With("artifactId", a.ArtifactID(), "type", a.ArtifactType(), "status", result.Status)
		if result.Error != nil {
			logger = logger.With("error", result.Error)
		}
		logger.Info("Artifact processed")

		return result.Error
	})
}

func (r *Runner) emitArtifact(a artifact.Artifact, status string, content any, err error) {
	report := job.ArtifactReport{
		ID:      a.ArtifactID(),
		Type:    a.ArtifactType(),
		Status:  status,
		Content: content,
	}
	if err != nil {
		report.Error = err.Error()
	}
	r.emitter.Emit(report)
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

// CheckReady checks if the ready marker file exists.
// Used by Docker health checks to determine when worker can start.
func CheckReady(sharedVolumePath string) bool {
	markerPath := filepath.Join(sharedVolumePath, ReadyFile)
	_, err := os.Stat(markerPath)
	return err == nil
}
