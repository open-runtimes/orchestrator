// Package docker implements the job.Orchestrator interface using the Docker API.
// Jobs run directly on the host Docker daemon.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/job"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Orchestrator implements job.Orchestrator using Docker.
type Orchestrator struct {
	client              *client.Client
	sidecarImage        string
	retentionPeriod     time.Duration
	maintenanceInterval time.Duration
	store               *job.Store
	emitter             *job.EventEmitter
	callbackProxyURL    string
	extraHosts          []string
	state               *stateRepo
	watcher             JobWatcher

	cancelMaintenance context.CancelFunc
	watchWg           sync.WaitGroup
}

// Config holds configuration for the Docker orchestrator.
type Config struct {
	SidecarImage        string
	RetentionPeriod     time.Duration // How long to keep completed jobs (default 15m)
	MaintenanceInterval time.Duration // How often to run cleanup (default 1m)
	CallbackProxyURL    string        // Internal URL for sidecar callbacks (e.g., http://host.docker.internal:8080)
	ExtraHosts          []string      // Extra /etc/hosts entries for containers (e.g., ["appwrite.test:host-gateway"])
}

// NewOrchestrator returns an OrchestratorFactory that creates a Docker orchestrator.
// The factory receives the shared Store and EventEmitter when called via job.NewOrchestrator.
// Register listeners on the emitter before calling Start.
func NewOrchestrator(ctx context.Context, cfg Config) job.OrchestratorFactory {
	return func(store *job.Store, emitter *job.EventEmitter) (job.Orchestrator, error) {
		dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, fmt.Errorf("failed to create docker client: %w", err)
		}

		retentionPeriod := cfg.RetentionPeriod
		if retentionPeriod <= 0 {
			retentionPeriod = 15 * time.Minute
		}

		maintenanceInterval := cfg.MaintenanceInterval
		if maintenanceInterval <= 0 {
			maintenanceInterval = 1 * time.Minute
		}

		return &Orchestrator{
			client:              dockerClient,
			sidecarImage:        cfg.SidecarImage,
			retentionPeriod:     retentionPeriod,
			maintenanceInterval: maintenanceInterval,
			store:               store,
			emitter:             emitter,
			callbackProxyURL:    cfg.CallbackProxyURL,
			extraHosts:          cfg.ExtraHosts,
			state:               newStateRepo(),
			watcher:             newDockerJobWatcher(dockerClient),
		}, nil
	}
}

// Start reconciles pre-existing jobs and begins background maintenance.
// Listeners must be registered via OnEvent before calling Start.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.reconcile(ctx); err != nil {
		slog.Warn("Failed to reconcile jobs", "error", err)
	}

	maintenanceCtx, cancel := context.WithCancel(context.Background())
	o.cancelMaintenance = cancel
	go o.runMaintenance(maintenanceCtx, o.maintenanceInterval)

	return nil
}

// reconcile scans Docker for existing job containers and resumes watching them.
// Handles various states:
// - Sidecar running, worker not started → check health and maybe start worker
// - Worker running → resume watching
// - Worker exited, sidecar running → signal sidecar
// - Both exited → mark completed
func (o *Orchestrator) reconcile(ctx context.Context) error {
	logger := slog.With("component", "reconcile")

	// Find all containers managed by this service
	containers, err := o.client.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "managed-by=jobs-service"),
		),
	})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	// Group containers by job ID
	type jobContainers struct {
		worker  *container.Summary
		sidecar *container.Summary
	}
	jobs := make(map[string]*jobContainers)

	for i := range containers {
		c := &containers[i]
		jobID := c.Labels["job.id"]
		if jobID == "" {
			continue
		}

		if jobs[jobID] == nil {
			jobs[jobID] = &jobContainers{}
		}

		switch c.Labels["job.type"] {
		case "worker":
			jobs[jobID].worker = c
		case "sidecar":
			jobs[jobID].sidecar = c
		}
	}

	// Rebuild state for each job
	var reconciled, resumed, completed int
	for jobID, jc := range jobs {
		// Must have at least sidecar
		if jc.sidecar == nil {
			logger.Warn("Job missing sidecar container", "jobId", jobID)
			continue
		}

		js := &jobState{
			sidecarContainerID: jc.sidecar.ID,
			volumeName:         fmt.Sprintf("job-%s-workspace", jobID),
		}

		if jc.worker != nil {
			js.jobContainerID = jc.worker.ID
		}

		o.state.commit(jobID, js)
		reconciled++

		// Determine job state and resume appropriately
		sidecarRunning := jc.sidecar.State == "running"
		workerRunning := jc.worker != nil && jc.worker.State == "running"

		switch {
		case !sidecarRunning && !workerRunning:
			// Both exited - job is completed
			completed++
			cs := inspectContainers(ctx, o.client, jobID, js.jobContainerID)
			exitCode := cs.workerExitCode
			if exitCode == 0 {
				_ = o.store.Set(jobID, job.StateCompleted, job.WithExitCode(exitCode))
			} else {
				_ = o.store.Set(jobID, job.StateFailed, job.WithExitCode(exitCode))
			}

		case sidecarRunning && jc.worker == nil:
			// Sidecar running, worker not created - shouldn't happen in normal flow
			logger.Warn("Job has sidecar but no worker container", "jobId", jobID)
			_ = o.store.Set(jobID, job.StateAccepted)

		case workerRunning:
			// Worker is running - resume watching
			_ = o.store.Set(jobID, job.StateRunning)
			resumed++
			cs := inspectContainers(ctx, o.client, jobID, js.jobContainerID)
			cfg := watchConfigFromState(cs)
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			js.cancelWatch = cancelWatch
			o.watchWg.Add(1)
			go func() {
				defer o.watchWg.Done()
				o.runWatchLoop(watchCtx, cfg, js)
			}()

		default:
			// Sidecar running with worker created but not running — accepted state
			_ = o.store.Set(jobID, job.StateAccepted)
			resumed++
			cs := inspectContainers(ctx, o.client, jobID, js.jobContainerID)
			cfg := watchConfigFromState(cs)
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			js.cancelWatch = cancelWatch
			o.watchWg.Add(1)
			go func() {
				defer o.watchWg.Done()
				o.runWatchLoop(watchCtx, cfg, js)
			}()
		}
	}

	logger.Info("Reconciliation complete", "reconciled", reconciled, "resumed", resumed, "completed", completed)
	return nil
}

// callbackDest holds destination info for dispatching events.
type callbackDest struct {
	jobID  string
	meta   map[string]string
	url    string
	key    string
	events []string
}

// Run creates and starts a job with its sidecar.
// The flow is event-driven:
// 1. Create volume, sidecar, and worker containers
// 2. Start sidecar (processes inputs, writes marker file)
// 3. Event watcher detects sidecar healthy → starts worker
// 4. Event watcher detects worker exit → signals sidecar
// 5. Event watcher detects sidecar exit → job complete
func (o *Orchestrator) Run(ctx context.Context, req *job.Request) error {
	if err := o.state.reserve(req.ID); err != nil {
		return err
	}
	if err := o.store.Set(req.ID, job.StateAccepted); err != nil {
		o.state.release(req.ID)
		return err
	}

	js := &jobState{
		volumeName: fmt.Sprintf("job-%s-workspace", req.ID),
	}

	// On failure, clean up resources and release reservation
	success := false
	defer func() {
		if !success {
			o.cleanup(ctx, js)
			o.store.Remove(req.ID)
			o.state.release(req.ID)
		}
	}()

	// Create shared volume
	if _, err := o.client.VolumeCreate(ctx, volume.CreateOptions{Name: js.volumeName}); err != nil {
		return apperrors.Internal("docker.createVolume", err)
	}

	// Pull job image if needed (with detached context so HTTP timeout doesn't cancel)
	pullCtx := context.WithoutCancel(ctx)
	if err := o.pullImageIfNeeded(pullCtx, req.Image); err != nil {
		return apperrors.Internal("docker.pullImage", err)
	}

	// Create job container (but don't start yet)
	var err error
	if js.jobContainerID, err = o.createJobContainer(ctx, req, js); err != nil {
		return apperrors.Internal("docker.createJobContainer", err)
	}

	// Create sidecar container
	if js.sidecarContainerID, err = o.createSidecarContainer(ctx, req, js); err != nil {
		return apperrors.Internal("docker.createSidecarContainer", err)
	}

	// Start sidecar (will process inputs and write marker file)
	if err := o.client.ContainerStart(ctx, js.sidecarContainerID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startSidecarContainer", err)
	}

	// Commit the job state
	o.state.commit(req.ID, js)
	success = true

	// Start event-driven watcher in background
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	js.cancelWatch = cancelWatch
	cfg := watchConfigFromRequest(req)
	o.watchWg.Add(1)
	go func() {
		defer o.watchWg.Done()
		o.runWatchLoop(watchCtx, cfg, js)
	}()

	return nil
}

// runWatchLoop drives job state transitions and callback emission from watcher events.
func (o *Orchestrator) runWatchLoop(ctx context.Context, cfg *watchConfig, js *jobState) {
	for e := range o.watcher.Watch(ctx, js.sidecarContainerID, js.jobContainerID) {
		switch ev := e.(type) {
		case SidecarReady:
			_ = o.store.Set(cfg.jobID, job.StateRunning)
			o.emitStartEvent(cfg)

		case WorkerExited:
			if ev.ExitCode == 0 {
				_ = o.store.Set(cfg.jobID, job.StateCompleted, job.WithExitCode(ev.ExitCode))
			} else {
				_ = o.store.Set(cfg.jobID, job.StateFailed, job.WithExitCode(ev.ExitCode))
			}
			o.sendExitEvent(cfg.jobID, cfg, ev.ExitCode, ev.Duration.Seconds())

		case SidecarExited:
			if !ev.WorkerEverStarted {
				_ = o.store.Set(cfg.jobID, job.StateFailed, job.WithExitCode(-1))
				o.sendExitEvent(cfg.jobID, cfg, -1, 0)
			}

		case LogLine:
			if cfg.dest == nil || !job.FilteredEvents(job.EventTypeLog, cfg.dest.events) {
				continue
			}
			builder := job.NewEventBuilder(cfg.jobID, "orchestrator/service", cfg.dest.meta)
			logEv := builder.BuildLogEvent(ev.Lines, ev.Stream)
			o.emitter.Emit(&job.Event{
				Payload:     logEv,
				CallbackURL: cfg.dest.url,
				SigningKey:  cfg.dest.key,
			})
		}
	}
}

// Stop stops a running job and cleans up its resources.
func (o *Orchestrator) Stop(ctx context.Context, jobID string) error {
	js, exists := o.state.release(jobID)
	if !exists {
		return apperrors.NotFound("job", jobID)
	}

	_ = o.store.Set(jobID, job.StateCancelled) // tolerate error if already terminal
	o.store.Remove(jobID)

	// Job is reserved but still initializing - nothing to clean up yet
	if js == nil {
		return nil
	}

	if js.cancelWatch != nil {
		js.cancelWatch()
	}

	o.cleanup(ctx, js)
	return nil
}

// Status returns the current status of a job.
func (o *Orchestrator) Status(ctx context.Context, jobID string) (*job.Status, error) {
	entry, exists := o.store.Get(jobID)
	if !exists {
		return nil, apperrors.NotFound("job", jobID)
	}
	return entry.Status(), nil
}

// List returns the status of all jobs.
func (o *Orchestrator) List(ctx context.Context) ([]job.Status, error) {
	entries := o.store.List()
	statuses := make([]job.Status, len(entries))
	for i, e := range entries {
		statuses[i] = *e.Status()
	}
	return statuses, nil
}

// Close releases resources held by the orchestrator.
func (o *Orchestrator) Close() error {
	if o.cancelMaintenance != nil {
		o.cancelMaintenance()
	}

	// Cancel all watch goroutines and wait for them to finish
	for _, js := range o.state.list() {
		if js != nil && js.cancelWatch != nil {
			js.cancelWatch()
		}
	}
	o.watchWg.Wait()

	return o.client.Close()
}

// Ready checks if the Docker daemon is reachable and responsive.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Ping(ctx)
	return err
}

// emitStartEvent emits the job start event to registered listeners.
func (o *Orchestrator) emitStartEvent(cfg *watchConfig) {
	if cfg.dest == nil {
		return
	}
	builder := job.NewEventBuilder(cfg.jobID, "orchestrator/service", cfg.dest.meta)
	event := builder.BuildStartEvent()
	if job.FilteredEvents(event.Type, cfg.dest.events) {
		o.emitter.Emit(&job.Event{
			Payload:     event,
			CallbackURL: cfg.dest.url,
			SigningKey:  cfg.dest.key,
		})
	}
}

// sendExitEvent emits the job exit event.
func (o *Orchestrator) sendExitEvent(jobID string, cfg *watchConfig, exitCode int, durationSeconds float64) {
	builder := job.NewEventBuilder(jobID, "orchestrator/service", nil)
	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("exit code %d", exitCode)
	}

	var callbackURL, signingKey string
	var eventFilter []string
	if cfg.dest != nil {
		builder = job.NewEventBuilder(jobID, "orchestrator/service", cfg.dest.meta)
		callbackURL = cfg.dest.url
		signingKey = cfg.dest.key
		eventFilter = cfg.dest.events
	}

	event := builder.BuildExitEvent(exitCode, cfg.image, durationSeconds, exitErr)
	if job.FilteredEvents(event.Type, eventFilter) {
		o.emitter.Emit(&job.Event{
			Payload:     event,
			CallbackURL: callbackURL,
			SigningKey:  signingKey,
		})
	}
}

func (o *Orchestrator) createJobContainer(ctx context.Context, req *job.Request, js *jobState) (string, error) {
	env := make([]string, 0, len(req.Environment))
	for k, v := range req.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	var cmd []string
	if req.Command != "" {
		cmd = []string{"/bin/sh", "-c", req.Command}
	}

	labels := map[string]string{
		"job.id":     req.ID,
		"job.type":   "worker",
		"managed-by": "jobs-service",
	}

	// Store original callback config as labels so it can be reconstructed
	// on resume without reading the sidecar's proxy-rewritten env vars.
	if req.Callback != nil && req.Callback.URL != "" {
		labels["job.callback.url"] = req.Callback.URL
		if req.Callback.Key != "" {
			labels["job.callback.key"] = req.Callback.Key
		}
		if len(req.Callback.Events) > 0 {
			labels["job.callback.events"] = strings.Join(req.Callback.Events, ",")
		}
	}
	if len(req.Meta) > 0 {
		if metaJSON, err := json.Marshal(req.Meta); err == nil {
			labels["job.meta"] = string(metaJSON)
		}
	}

	containerConfig := &container.Config{
		Image:      req.Image,
		Cmd:        cmd,
		Env:        env,
		WorkingDir: req.Workspace,
		Labels:     labels,
	}

	hostConfig := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: js.volumeName,
				Target: req.Workspace,
			},
		},
		Resources: container.Resources{
			NanoCPUs: int64(req.CPU * 1e9),
			Memory:   int64(req.Memory) * 1024 * 1024,
		},
	}

	containerName := fmt.Sprintf("job-%s-worker", req.ID)
	resp, err := o.client.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (o *Orchestrator) createSidecarContainer(ctx context.Context, req *job.Request, js *jobState) (string, error) {
	env := []string{
		fmt.Sprintf("JOB_ID=%s", req.ID),
		fmt.Sprintf("TIMEOUT_SECONDS=%d", req.TimeoutSeconds),
		fmt.Sprintf("SHARED_VOLUME_PATH=%s", req.Workspace),
	}

	if len(req.Artifacts) > 0 {
		artifactsJSON, err := json.Marshal(req.Artifacts)
		if err != nil {
			return "", fmt.Errorf("failed to marshal artifacts: %w", err)
		}
		env = append(env, fmt.Sprintf("ARTIFACTS_JSON=%s", string(artifactsJSON)))
	}

	if req.Callback != nil && req.Callback.URL != "" {
		callbackURL := req.Callback.URL
		// If proxy URL is configured, route callbacks through orchestrator
		if o.callbackProxyURL != "" {
			callbackURL = fmt.Sprintf("%s/internal/events?url=%s",
				o.callbackProxyURL,
				url.QueryEscape(req.Callback.URL),
			)
		}
		env = append(env, fmt.Sprintf("CALLBACK_URL=%s", callbackURL))
		if req.Callback.Key != "" {
			// Sidecar signs events with this key
			env = append(env, fmt.Sprintf("CALLBACK_KEY=%s", req.Callback.Key))
		}
		if len(req.Callback.Events) > 0 {
			env = append(env, fmt.Sprintf("CALLBACK_EVENTS=%s", strings.Join(req.Callback.Events, ",")))
		}
	}

	if len(req.Meta) > 0 {
		metaJSON, err := json.Marshal(req.Meta)
		if err != nil {
			return "", fmt.Errorf("failed to marshal meta: %w", err)
		}
		env = append(env, fmt.Sprintf("JOB_META=%s", string(metaJSON)))
	}

	// Health check via sidecar binary checking for marker file
	// Docker will emit health_status events when this passes
	healthCheck := &container.HealthConfig{
		Test:        []string{"CMD", "/ko-app/job-sidecar", "-check-ready"},
		Interval:    200 * time.Millisecond,
		Timeout:     5 * time.Second,
		StartPeriod: time.Duration(req.TimeoutSeconds) * time.Second,
		Retries:     0, // Immediate success on first pass
	}

	containerConfig := &container.Config{
		Image:       o.sidecarImage,
		Env:         env,
		User:        "0", // Run as root to write to shared volume
		Healthcheck: healthCheck,
		Labels: map[string]string{
			"job.id":     req.ID,
			"job.type":   "sidecar",
			"managed-by": "jobs-service",
		},
	}

	hostConfig := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: js.volumeName,
				Target: req.Workspace,
			},
		},
		ExtraHosts: o.extraHosts,
	}

	containerName := fmt.Sprintf("job-%s-sidecar", req.ID)
	resp, err := o.client.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (o *Orchestrator) pullImageIfNeeded(ctx context.Context, imageName string) error {
	_, err := o.client.ImageInspect(ctx, imageName)
	if err == nil {
		return nil
	}

	reader, err := o.client.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()

	_, err = io.Copy(io.Discard, reader)
	return err
}

func (o *Orchestrator) cleanup(ctx context.Context, js *jobState) {
	const stopTimeout = 10

	o.removeContainer(ctx, js.sidecarContainerID, stopTimeout)
	o.removeContainer(ctx, js.jobContainerID, stopTimeout)

	if js.volumeName != "" {
		_ = o.client.VolumeRemove(ctx, js.volumeName, true)
	}
}

func (o *Orchestrator) removeContainer(ctx context.Context, containerID string, stopTimeout int) {
	if containerID == "" {
		return
	}
	_ = o.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &stopTimeout})
	_ = o.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// runMaintenance periodically cleans up expired completed jobs.
func (o *Orchestrator) runMaintenance(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.cleanupExpiredJobs(ctx)
		}
	}
}

// isTerminal returns true if the state has no outgoing FSM transitions.
func isTerminal(state string) bool {
	return state == job.StateCompleted || state == job.StateFailed || state == job.StateCancelled
}

// cleanupExpiredJobs removes jobs that completed more than retentionPeriod ago.
func (o *Orchestrator) cleanupExpiredJobs(ctx context.Context) {
	now := time.Now()
	logger := slog.With("component", "maintenance")

	entries := o.store.List()
	var expired []string
	for _, e := range entries {
		if isTerminal(e.State) && now.Sub(e.UpdatedAt) > o.retentionPeriod {
			expired = append(expired, e.ID)
		}
	}

	if len(expired) == 0 {
		return
	}

	for _, jobID := range expired {
		if js, exists := o.state.release(jobID); exists && js != nil {
			o.cleanup(ctx, js)
		}
		o.store.Remove(jobID)
		logger.Debug("Cleaned up expired job", "jobId", jobID)
	}

	logger.Info("Maintenance complete", "cleaned", len(expired))
}

// Verify Orchestrator implements job.Orchestrator
var _ job.Orchestrator = (*Orchestrator)(nil)
