package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/pkg/emitter"
	"orchestrator/pkg/job"
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

// defaultPostJobFileGrace bounds how long a post-job artifact waits for its
// source file to appear. The worker has already exited by then, so the file
// either exists or never will — a missing file must fail fast instead of
// parking the job (and its complete callback) on the full job timeout.
const defaultPostJobFileGrace = 10 * time.Second

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

// WithMounter replaces the default platform Mounter. Used in tests to inject a
// fake instead of performing real loop mounts.
func WithMounter(m Mounter) Option {
	return func(r *Runner) { r.mounter = m }
}

// WithPostJobFileGrace overrides how long post-job artifacts wait for their
// source file to appear. Used in tests to avoid real waits.
func WithPostJobFileGrace(d time.Duration) Option {
	return func(r *Runner) { r.postFileGrace = d }
}

// WithS3Credentials sets the credentials used to sign s3:// download/upload
// artifacts. Configured per service (jobs vs deployments) and forwarded by the
// orchestrator into this sidecar's environment.
func WithS3Credentials(creds config.S3Credentials) Option {
	return func(r *Runner) { r.s3 = creds }
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
	mounter          Mounter
	mounted          []string // mount targets to unmount on teardown
	emitter          emitter.Emitter[job.ArtifactReport]
	s3               config.S3Credentials
	postFileGrace    time.Duration // wait for a post-job artifact's source file
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
		mounter:          defaultMounter(),
		postFileGrace:    defaultPostJobFileGrace,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// NewHTTPSink returns an emitter listener that POSTs artifact results to the orchestrator.
// It captures all job-level fields (callback config, meta) and merges them into each report.
// The HTTP client timeout governs per-request cancellation.
func NewHTTPSink(jobID, endpoint, token string, timeout time.Duration, callbackURL, callbackKey string, callbackEvents []string, meta map[string]string) func(job.ArtifactReport) {
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
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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

// Run executes the Docker-style sidecar flow:
// 1. Process pre-job artifacts (downloads, file writes, etc.)
// 2. Write the ready marker so Docker's health check starts the worker
// 3. Wait for completion signal (SIGUSR1 from Docker, SIGTERM from Kubernetes)
// 4. Process post-job artifacts (uploads, events, etc.)
//
// If any pre-job artifact fails, the sidecar exits with an error.
func (r *Runner) Run(ctx context.Context, artifacts []artifact.Artifact) error {
	mounts, rest := splitMounts(artifacts)
	preJob, postJob := artifact.Partition(rest)

	logger := slog.With("jobId", r.jobID, "preJob", len(preJob), "mounts", len(mounts), "postJob", len(postJob))
	logger.Info("Sidecar starting")

	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.timeoutSeconds)*time.Second)
	defer cancel()

	if err := r.processArtifacts(ctx, preJob, false); err != nil {
		logger.Error("Pre-job artifact processing failed, aborting job", "error", err)
		return fmt.Errorf("pre-job artifact processing failed: %w", err)
	}

	if err := r.establishMounts(ctx, mounts); err != nil {
		r.unmountAll() // roll back any mounts established before the failure
		logger.Error("Mount setup failed, aborting job", "error", err)
		return fmt.Errorf("mount setup failed: %w", err)
	}

	if err := r.writeReadyMarker(); err != nil {
		logger.Error("Failed to write ready marker", "error", err)
		return err
	}

	logger.Info("Waiting for worker completion signal")
	r.waitFn(ctx)
	logger.Info("Received worker completion signal")

	if err := r.processArtifacts(ctx, postJob, true); err != nil {
		logger.Warn("Post-job artifact processing failed", "error", err)
	}

	r.unmountAll()
	logger.Info("Sidecar completed")
	return nil
}

// RunPre processes pre-job artifacts and exits. Used by the Kubernetes backend as
// a regular init container — the worker will not start until this returns successfully.
func (r *Runner) RunPre(ctx context.Context, artifacts []artifact.Artifact) error {
	_, rest := splitMounts(artifacts) // mounts are established by the post sidecar
	preJob, _ := artifact.Partition(rest)
	logger := slog.With("jobId", r.jobID, "mode", "pre", "preJob", len(preJob))
	logger.Info("Sidecar pre-mode starting")

	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.timeoutSeconds)*time.Second)
	defer cancel()

	if err := r.processArtifacts(ctx, preJob, false); err != nil {
		logger.Error("Pre-job artifact processing failed, aborting job", "error", err)
		return fmt.Errorf("pre-job artifact processing failed: %w", err)
	}
	logger.Info("Sidecar pre-mode completed")
	return nil
}

// RunPost waits for the worker to finish, then processes post-job artifacts.
// Used by the Kubernetes backend as a native sidecar container — kubelet sends
// SIGTERM when the worker main container exits, which unblocks waitFn.
func (r *Runner) RunPost(ctx context.Context, artifacts []artifact.Artifact) error {
	mounts, rest := splitMounts(artifacts)
	_, postJob := artifact.Partition(rest)
	logger := slog.With("jobId", r.jobID, "mode", "post", "mounts", len(mounts), "postJob", len(postJob))

	// Establish mounts at startup, before signaling ready, so they exist when
	// the worker starts. The startup probe gates the worker on the marker.
	if len(mounts) > 0 {
		logger.Info("Establishing squashfs mounts")
		mountCtx, cancel := context.WithTimeout(context.Background(), time.Duration(r.timeoutSeconds)*time.Second)
		err := r.establishMounts(mountCtx, mounts)
		cancel()
		if err != nil {
			r.unmountAll() // roll back any mounts established before the failure
			logger.Error("Mount setup failed, aborting job", "error", err)
			return fmt.Errorf("mount setup failed: %w", err)
		}
		if err := r.writeMountReadyMarker(); err != nil {
			logger.Error("Failed to write mounts-ready marker", "error", err)
			return err
		}
	}

	logger.Info("Waiting for worker to finish")
	r.waitFn(ctx)
	logger.Info("Worker finished, processing post-job artifacts")

	// Use a detached context with timeout so a parent cancellation (e.g. from
	// the SIGTERM we just received) does not short-circuit post-artifact work.
	postCtx, cancel := context.WithTimeout(context.Background(), time.Duration(r.timeoutSeconds)*time.Second)
	defer cancel()

	if err := r.processArtifacts(postCtx, postJob, true); err != nil {
		logger.Warn("Post-job artifact processing failed", "error", err)
	}

	r.unmountAll()
	logger.Info("Sidecar post-mode completed")
	return nil
}

func (r *Runner) writeReadyMarker() error {
	markerPath := filepath.Join(r.sharedVolumePath, ReadyFile)
	if err := os.WriteFile(markerPath, []byte{}, 0o644); err != nil {
		return fmt.Errorf("failed to write ready marker: %w", err)
	}
	return nil
}

func (r *Runner) writeMountReadyMarker() error {
	markerPath := filepath.Join(r.sharedVolumePath, MountReadyFile)
	if err := os.WriteFile(markerPath, []byte{}, 0o644); err != nil {
		return fmt.Errorf("failed to write mounts-ready marker: %w", err)
	}
	return nil
}

// establishMounts mounts each squashfs image read-only into the workspace. A
// failure aborts the job — the worker must not start without its inputs.
func (r *Runner) establishMounts(ctx context.Context, mounts []artifact.Artifact) error {
	for _, a := range mounts {
		m, ok := a.(*artifact.Mount)
		if !ok {
			continue
		}
		image := filepath.Join(r.sharedVolumePath, m.In)
		target := filepath.Join(r.sharedVolumePath, m.Out)

		err := r.waitForPath(ctx, image)
		if err == nil {
			err = os.MkdirAll(target, 0o755)
		}
		if err == nil {
			err = r.mounter.Mount(image, target, MountOpts{Writable: m.Writable, SizeMiB: m.Size})
		}
		if err != nil {
			r.emitArtifact(a, "failed", nil, err)
			slog.With("artifactId", m.ID, "error", err).Error("Mount failed")
			return fmt.Errorf("mount %s: %w", m.ID, err)
		}

		r.mounted = append(r.mounted, target)
		r.emitArtifact(a, "success", nil, nil)
		slog.With("artifactId", m.ID, "image", m.In, "target", m.Out).Info("Mounted squashfs image")
	}
	return nil
}

// unmountAll tears down established mounts in reverse order (best effort).
func (r *Runner) unmountAll() {
	for i := len(r.mounted) - 1; i >= 0; i-- {
		if err := r.mounter.Unmount(r.mounted[i]); err != nil {
			slog.With("target", r.mounted[i], "error", err).Warn("Failed to unmount")
		}
	}
	r.mounted = nil
}

// processArtifacts processes artifacts in dependency order.
// For post-job artifacts, it waits for files to appear before processing.
// s3Configurable is implemented by artifacts that transfer over s3:// and need
// SigV4 credentials. Download and Upload satisfy it; artifacts that never touch
// S3 do not, so the runner injects credentials only where they are used.
type s3Configurable interface {
	SetS3Credentials(config.S3Credentials)
}

func (r *Runner) processArtifacts(ctx context.Context, artifacts []artifact.Artifact, waitForFiles bool) error {
	return artifact.RunInOrder(ctx, artifacts, func(ctx context.Context, a artifact.Artifact) error {
		if c, ok := a.(s3Configurable); ok {
			c.SetS3Credentials(r.s3)
		}
		if waitForFiles {
			if srcPath := r.registry.SourcePath(a); srcPath != "" {
				fullPath := filepath.Join(r.sharedVolumePath, srcPath)
				// In Run() the parent ctx carries the job timeout, so the
				// actual window is min(remaining job time, grace) — report the
				// measured wait, not the configured grace.
				start := time.Now()
				waitCtx, cancel := context.WithTimeout(ctx, r.postFileGrace)
				err := r.waitForPath(waitCtx, fullPath)
				cancel()
				if err != nil {
					err = fmt.Errorf("%s did not appear within %s of worker exit: %w", srcPath, time.Since(start).Round(time.Millisecond), err)
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
		report.FailureReason = err.Error()
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
