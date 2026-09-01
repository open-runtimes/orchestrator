package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/emitter"
	"orchestrator/internal/job"
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

// defaultTimeoutSeconds mirrors the TIMEOUT_SECONDS config default and bounds
// individual artifact phases when the job itself is unbounded.
const defaultTimeoutSeconds = 1800

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
	sync             syncState     // delta sync loops for synced mounts
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

// Run executes the combined sidecar flow:
// 1. Process pre-job artifacts (downloads, file writes, etc.)
// 2. Establish mounts, then write the ready marker that gates the worker
// 3. Wait for completion signal (SIGUSR1 from Docker, SIGTERM from Kubernetes)
// 4. Process post-job artifacts (uploads, events, etc.)
//
// If any pre-job artifact fails, the sidecar exits with an error.
//
// A container restart recovers in place, which is what makes this flow safe as
// a Kubernetes native sidecar: completed downloads of immutable sources are
// kept (Download's skipIfExists), surviving mounts are adopted, and the ready
// marker is cleared up front so the worker is only ever admitted behind
// established mounts. On a fresh workspace every recovery step is a no-op.
func (r *Runner) Run(ctx context.Context, artifacts []artifact.Artifact) error {
	mounts, rest := splitMounts(artifacts)
	preJob, postJob := artifact.Partition(rest)

	logger := slog.With("jobId", r.jobID, "preJob", len(preJob), "mounts", len(mounts), "postJob", len(postJob), "timeoutSeconds", r.timeoutSeconds)
	logger.Info("Sidecar starting")

	if err := r.removeReadyMarker(); err != nil {
		return err
	}

	setupCtx, cancel := context.WithTimeout(ctx, r.phaseTimeout())
	err := r.processArtifacts(setupCtx, preJob, false)
	cancel()
	if err != nil {
		logger.Error("Pre-job artifact processing failed, aborting job", "error", err)
		return fmt.Errorf("pre-job artifact processing failed: %w", err)
	}

	adopted, err := r.adoptExistingMounts(mounts)
	if err != nil {
		return fmt.Errorf("mount adoption failed: %w", err)
	}
	if adopted {
		// The overlay (and any restored upper) survived the restart; only the
		// sync loops died with the process.
		logger.Info("Adopted mounts from a previous incarnation")
		for _, a := range mounts {
			if m, ok := a.(*artifact.Mount); ok && m.Sync != "" {
				r.startSync(m)
			}
		}
	} else if err := r.Mount(ctx, artifacts); err != nil {
		logger.Error("Mount setup failed, aborting job", "error", err)
		return fmt.Errorf("mount setup failed: %w", err)
	}

	if err := r.writeReadyMarker(); err != nil {
		// The worker will never be admitted, so the mounts (and any sync
		// loops) established above have no consumer — tear them down rather
		// than leak them past this incarnation.
		r.Release()
		logger.Error("Failed to write ready marker", "error", err)
		return err
	}

	// A bounded job's wait carries the job deadline — if the completion signal
	// never arrives, this is what unsticks the sidecar. An unbounded workload
	// (timeout 0) waits on the caller's context alone: the pod, not a job
	// deadline, decides when it dies, and kubelet's SIGTERM ends the hold —
	// a deadline there would tear the mounts out from under a serving workload.
	holdCtx := ctx
	if r.timeoutSeconds > 0 {
		var cancelHold context.CancelFunc
		holdCtx, cancelHold = context.WithTimeout(ctx, r.phaseTimeout())
		defer cancelHold()
	}
	logger.Info("Waiting for worker completion signal")
	r.waitFn(holdCtx)
	logger.Info("Received worker completion signal")

	// Detached and bounded, like RunPost: the completion signal may be the
	// SIGTERM that also cancelled the caller's context, and post-job work
	// must still fit inside the termination grace period.
	postCtx, cancelPost := context.WithTimeout(context.Background(), r.phaseTimeout())
	defer cancelPost()
	if err := r.processArtifacts(postCtx, postJob, true); err != nil {
		logger.Warn("Post-job artifact processing failed", "error", err)
	}

	r.Release()
	logger.Info("Sidecar completed")
	return nil
}

// RunPre processes pre-job artifacts and exits. Used by the Kubernetes backend as
// a regular init container — the worker will not start until this returns successfully.
//
// Mounts are skipped here, not dropped: on a job the post sidecar in the same
// pod establishes them before the worker starts, so this phase must leave them
// alone. A consumer with no post sidecar cannot honour a mount at all, and says
// so before it ever gets here — the serving registry rejects the type at
// validation, and the claim endpoint refuses it (internal/proxy/pool.go).
func (r *Runner) RunPre(ctx context.Context, artifacts []artifact.Artifact) error {
	_, rest := splitMounts(artifacts)
	preJob, _ := artifact.Partition(rest)
	logger := slog.With("jobId", r.jobID, "mode", "pre", "preJob", len(preJob))
	logger.Info("Sidecar pre-mode starting")

	ctx, cancel := context.WithTimeout(ctx, r.phaseTimeout())
	defer cancel()

	if err := r.processArtifacts(ctx, preJob, false); err != nil {
		logger.Error("Pre-job artifact processing failed, aborting job", "error", err)
		return fmt.Errorf("pre-job artifact processing failed: %w", err)
	}
	logger.Info("Sidecar pre-mode completed")
	return nil
}

// Mount establishes the mounts among artifacts, for a consumer that runs
// no post phase but holds its workload for as long as the pod lives: a claimed
// warm pod. Call it after the pre phase has materialized the images and BEFORE
// the workload is signalled, so the mount is already there when it execs.
//
// Release must run on shutdown. The workspace carries bidirectional
// propagation, so a mount left behind outlives the pod on its node.
func (r *Runner) Mount(ctx context.Context, artifacts []artifact.Artifact) error {
	mounts, _ := splitMounts(artifacts)
	if len(mounts) == 0 {
		return nil
	}
	logger := slog.With("jobId", r.jobID, "mounts", len(mounts))
	logger.Info("Establishing artifact mounts")

	// Bounded, like every other phase: the image was materialized by the pre
	// phase that just ran, so a missing one is never going to arrive. Without a
	// deadline the wait would hang the claim request — and hold the pod claimed
	// — rather than failing it.
	ctx, cancel := context.WithTimeout(ctx, r.phaseTimeout())
	defer cancel()

	// A synced mount is restored before its overlay is stacked: the delta has to
	// be in the upper layer when the workload first looks, not merged in after.
	for _, a := range mounts {
		m, ok := a.(*artifact.Mount)
		if !ok || m.Sync == "" {
			continue
		}
		if err := r.restoreDelta(ctx, m); err != nil {
			r.unmountAll()
			return fmt.Errorf("mount %s: %w", m.ID, err)
		}
	}

	if err := r.establishMounts(ctx, mounts); err != nil {
		r.unmountAll() // roll back whatever was established before the failure
		return err
	}

	// Now that the overlay is up, keep pushing what the workload changes. Stops
	// (and flushes) in Release.
	for _, a := range mounts {
		if m, ok := a.(*artifact.Mount); ok && m.Sync != "" {
			r.startSync(m)
		}
	}
	logger.Info("Artifact mounts established")
	return nil
}

// Release stops the sync loops, flushes each synced mount once more, and then
// unmounts everything Mount established, innermost first. The order matters: the
// delta is read through the upper layer, which unmounting takes away. Safe to
// call when nothing was mounted.
//
// The flush is bounded by the caller's shutdown budget. Missing it costs the
// last sync interval rather than the session, which is the whole reason the sync
// runs continuously.
func (r *Runner) Release() {
	ctx, cancel := context.WithTimeout(context.Background(), r.phaseTimeout())
	defer cancel()
	r.StopSync(ctx)
	r.unmountAll()
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
		logger.Info("Establishing artifact mounts")
		if err := r.removeMountReadyMarker(); err != nil {
			return err
		}
		mountCtx, cancel := context.WithTimeout(context.Background(), r.phaseTimeout())
		adopted, err := r.adoptExistingMounts(mounts)
		if err == nil && !adopted {
			err = r.establishMounts(mountCtx, mounts)
		}
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
	postCtx, cancel := context.WithTimeout(context.Background(), r.phaseTimeout())
	defer cancel()

	if err := r.processArtifacts(postCtx, postJob, true); err != nil {
		logger.Warn("Post-job artifact processing failed", "error", err)
	}

	r.unmountAll()
	logger.Info("Sidecar post-mode completed")
	return nil
}

// phaseTimeout bounds an individual artifact phase (downloads, mounts, the
// post-job flush). It is the job timeout when the job is bounded; an unbounded
// workload (timeoutSeconds == 0) still must not hang a phase forever, so
// phases fall back to the default budget.
func (r *Runner) phaseTimeout() time.Duration {
	if r.timeoutSeconds > 0 {
		return time.Duration(r.timeoutSeconds) * time.Second
	}
	return defaultTimeoutSeconds * time.Second
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

// removeReadyMarker clears a ready marker left in the shared volume by a
// previous incarnation of a runtime-mode sidecar, so the app container's
// startup probe only passes once this incarnation's mounts are established.
func (r *Runner) removeReadyMarker() error {
	markerPath := filepath.Join(r.sharedVolumePath, ReadyFile)
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale ready marker: %w", err)
	}
	return nil
}

func (r *Runner) removeMountReadyMarker() error {
	markerPath := filepath.Join(r.sharedVolumePath, MountReadyFile)
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale mounts-ready marker: %w", err)
	}
	return nil
}

// adoptExistingMounts recovers ownership after a Kubernetes native-sidecar
// restart. Mounts live in the pod's propagated mount namespace and can outlive
// the container process that established them. If every requested target is
// still mounted, track them so normal teardown unmounts them. A partial set is
// stale state: tear it down before establishing the complete declaration.
func (r *Runner) adoptExistingMounts(mounts []artifact.Artifact) (bool, error) {
	targets := make([]string, 0, len(mounts))
	mounted := make([]bool, 0, len(mounts))
	count := 0
	for _, a := range mounts {
		m, ok := a.(*artifact.Mount)
		if !ok {
			continue
		}
		target := filepath.Join(r.sharedVolumePath, m.Out)
		active, err := r.mounter.IsMounted(target)
		if err != nil {
			return false, fmt.Errorf("inspect mount %s: %w", m.ID, err)
		}
		targets = append(targets, target)
		mounted = append(mounted, active)
		if active {
			count++
		}
	}

	if count == len(targets) && count > 0 {
		r.mounted = append(r.mounted, targets...)
		return true, nil
	}
	for i := len(targets) - 1; i >= 0; i-- {
		if mounted[i] {
			if err := r.mounter.Unmount(targets[i]); err != nil {
				return false, fmt.Errorf("remove partial mount %s: %w", targets[i], err)
			}
		}
	}
	return false, nil
}

// establishMounts mounts each image read-only into the workspace. A failure
// aborts the job — the worker must not start without its inputs.
func (r *Runner) establishMounts(ctx context.Context, mounts []artifact.Artifact) error {
	for _, a := range mounts {
		m, ok := a.(*artifact.Mount)
		if !ok {
			continue
		}
		image := filepath.Join(r.sharedVolumePath, m.In)
		target := filepath.Join(r.sharedVolumePath, m.Out)

		start := time.Now()
		err := r.waitForPath(ctx, image)
		if err == nil {
			err = os.MkdirAll(target, 0o755)
		}

		format, compression := "", ""
		if err == nil {
			format, compression, err = artifact.ClassifyFile(image)
		}

		source := image
		sourceDir := false
		if err == nil && format == "tar" {
			lowerRel := filepath.Join(m.Out, ".lower")
			source, err = r.extractTarMountLower(ctx, m, lowerRel)
			if err == nil {
				sourceDir = true
			}
		}
		if err == nil {
			err = r.mounter.Mount(source, target, MountOpts{
				Writable: m.Writable, SizeMiB: m.Size,
				SourceDir: sourceDir,
				// A synced delta must outlive each write and be readable by the
				// artifact runner, which a tmpfs upper is not.
				UpperOnDisk: m.Sync != "",
			})
			if err != nil && sourceDir {
				_ = os.RemoveAll(source)
			}
		}
		if err != nil {
			r.emitArtifact(a, artifact.Result{Status: "failed", Error: err}, start)
			slog.With("artifactId", m.ID, "error", err).Error("Mount failed")
			return fmt.Errorf("mount %s: %w", m.ID, err)
		}

		r.mounted = append(r.mounted, target)
		r.emitArtifact(a, artifact.Result{Status: "success", Format: format, Compression: compression}, start)
		slog.With("artifactId", m.ID, "image", m.In, "target", m.Out, "format", format, "compression", compression).Info("Mounted image")
	}
	return nil
}

// extractTarMountLower materializes a tar into the implementation directory
// used as a bind or overlay lower. Writable overlays must also be able to copy
// up existing entries: overlayfs checks each lower inode's permissions first,
// and extraction made those inodes root-owned because the sidecar runs as root.
func (r *Runner) extractTarMountLower(ctx context.Context, m *artifact.Mount, lowerRel string) (string, error) {
	lower := filepath.Join(r.sharedVolumePath, lowerRel)
	if err := os.RemoveAll(lower); err != nil {
		return "", fmt.Errorf("remove stale tar lower directory: %w", err)
	}
	if err := os.Mkdir(lower, 0o755); err != nil {
		return "", fmt.Errorf("create tar lower directory: %w", err)
	}

	result := (&artifact.Unarchive{In: m.In, Out: lowerRel}).Apply(ctx, r.sharedVolumePath)
	if result.Error != nil {
		_ = os.RemoveAll(lower)
		return "", result.Error
	}
	if m.Writable {
		// Open the extracted tree to the unknown workload uid just like a
		// restored sync delta and the overlay upper directory.
		if err := makeWritable(lower); err != nil {
			_ = os.RemoveAll(lower)
			return "", fmt.Errorf("make tar lower writable: %w", err)
		}
	}
	return lower, nil
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
		start := time.Now()
		if c, ok := a.(s3Configurable); ok {
			c.SetS3Credentials(r.s3)
		}
		if waitForFiles {
			if srcPath := r.registry.SourcePath(a); srcPath != "" {
				fullPath := filepath.Join(r.sharedVolumePath, srcPath)
				// In Run() the parent ctx carries the job timeout, so the
				// actual window is min(remaining job time, grace) — report the
				// measured wait, not the configured grace.
				waitCtx, cancel := context.WithTimeout(ctx, r.postFileGrace)
				err := r.waitForPath(waitCtx, fullPath)
				cancel()
				if err != nil {
					err = fmt.Errorf("%s did not appear within %s of worker exit: %w", srcPath, time.Since(start).Round(time.Millisecond), err)
					r.emitArtifact(a, artifact.Result{Status: "failed", Error: err}, start)
					slog.With("artifactId", a.ArtifactID(), "error", err).Warn("Artifact failed (file not found)")
					return err
				}
			}
		}

		result := a.Apply(ctx, r.sharedVolumePath)
		r.emitArtifact(a, *result, start)

		logger := slog.With("artifactId", a.ArtifactID(), "type", a.ArtifactType(), "status", result.Status)
		if result.Error != nil {
			logger = logger.With("error", result.Error)
		}
		logger.Info("Artifact processed")

		return result.Error
	})
}

func (r *Runner) emitArtifact(a artifact.Artifact, res artifact.Result, start time.Time) {
	report := job.ArtifactReport{
		ID:              a.ArtifactID(),
		Type:            a.ArtifactType(),
		Status:          res.Status,
		Content:         res.Content,
		Format:          res.Format,
		Compression:     res.Compression,
		DurationSeconds: time.Since(start).Seconds(),
	}
	if res.Error != nil {
		report.FailureReason = res.Error.Error()
	}
	r.emitter.Emit(report)
}

func (r *Runner) waitForPath(ctx context.Context, path string) error {
	// The path is usually already there (the dependency artifact completed
	// before this one started), so check before the first tick — waiting a
	// full period first taxed every mount-mode cold start 100ms for nothing.
	if _, err := os.Stat(path); err == nil {
		return nil
	}

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
