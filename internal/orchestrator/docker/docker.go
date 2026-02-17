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
	"github.com/docker/docker/api/types/events"
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
		workerCreated := jc.worker != nil

		switch {
		case !sidecarRunning && !workerRunning:
			// Both exited - job is completed
			completed++
			cs := inspectContainers(ctx, o.client, jobID, js.jobContainerID, js.sidecarContainerID)
			exitCode := cs.workerExitCode
			if exitCode == 0 {
				_ = o.store.Set(jobID, job.StateCompleted, job.WithExitCode(exitCode))
			} else {
				_ = o.store.Set(jobID, job.StateFailed, job.WithExitCode(exitCode))
			}

		case sidecarRunning && !workerCreated:
			// Sidecar running, worker not created - shouldn't happen in normal flow
			logger.Warn("Job has sidecar but no worker container", "jobId", jobID)
			_ = o.store.Set(jobID, job.StateAccepted)

		case workerRunning:
			// Worker is running
			_ = o.store.Set(jobID, job.StateRunning)
			resumed++
			cs := inspectContainers(ctx, o.client, jobID, js.jobContainerID, js.sidecarContainerID)
			cfg, watchState := watchConfigFromState(cs)
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			js.cancelWatch = cancelWatch
			o.watchWg.Add(1)
			go func() {
				defer o.watchWg.Done()
				o.watchJobEvents(watchCtx, cfg, js, watchState)
			}()

		default:
			// Sidecar running with worker created but not running — accepted state
			_ = o.store.Set(jobID, job.StateAccepted)
			resumed++
			cs := inspectContainers(ctx, o.client, jobID, js.jobContainerID, js.sidecarContainerID)
			cfg, watchState := watchConfigFromState(cs)
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			js.cancelWatch = cancelWatch
			o.watchWg.Add(1)
			go func() {
				defer o.watchWg.Done()
				o.watchJobEvents(watchCtx, cfg, js, watchState)
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
		o.watchJobEvents(watchCtx, cfg, js, &jobWatchState{})
	}()

	return nil
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

// jobWatchState tracks the state of a job being watched.
type jobWatchState struct {
	workerStarted bool
	workerExited  bool
	startTime     time.Time
	logCancel     context.CancelFunc
	logDone       chan struct{}
}

// watchJobEvents uses Docker events to coordinate the job lifecycle:
// 1. Sidecar healthy → start worker
// 2. Worker exit → signal sidecar
// 3. Sidecar exit → job complete
//
// Used for both new and resumed jobs. The initial state parameter allows
// pre-seeding worker status from container inspection during reconciliation.
// The watcher reconnects on event stream errors after reconciling current state.
func (o *Orchestrator) watchJobEvents(ctx context.Context, cfg *watchConfig, js *jobState, state *jobWatchState) {
	logger := slog.With("jobId", cfg.jobID)

	// Reconnection loop
	for {
		// Check if context is cancelled
		if ctx.Err() != nil {
			o.cleanupLogStreaming(state)
			return
		}

		// Subscribe to events FIRST (before checking state)
		// This prevents race where health event fires between state check and subscribe
		eventFilter := filters.NewArgs(
			filters.Arg("type", string(events.ContainerEventType)),
			filters.Arg("container", js.sidecarContainerID),
			filters.Arg("container", js.jobContainerID),
		)
		eventCh, errCh := o.client.Events(ctx, events.ListOptions{Filters: eventFilter})

		// NOW reconcile current state (in case we missed events before subscribing)
		done := o.reconcileJobState(ctx, logger, cfg, js, state)
		if done {
			return
		}

		// Process events until error or completion
		done = o.processJobEvents(ctx, logger, cfg, js, state, eventCh, errCh)
		if done {
			return
		}

		// Event stream error - wait before reconnecting
		logger.Warn("Event stream disconnected, reconnecting...")
		select {
		case <-ctx.Done():
			o.cleanupLogStreaming(state)
			return
		case <-time.After(time.Second):
			// Continue to reconnect
		}
	}
}

// reconcileJobState checks current container states and updates watch state.
// Returns true if the job is complete and watching should stop.
func (o *Orchestrator) reconcileJobState(ctx context.Context, logger *slog.Logger, cfg *watchConfig, js *jobState, state *jobWatchState) bool {
	// Check sidecar state
	sidecarInspect, err := o.client.ContainerInspect(ctx, js.sidecarContainerID)
	if err != nil {
		logger.Error("Failed to inspect sidecar during reconcile", "error", err)
		return true // Can't continue without sidecar info
	}

	// If sidecar has exited, job is done
	if !sidecarInspect.State.Running {
		if !state.workerStarted {
			logger.Error("Sidecar exited before inputs completed")
			_ = o.store.Set(cfg.jobID, job.StateFailed, job.WithExitCode(-1))
			o.sendExitEvent(cfg.jobID, cfg, -1, 0)
		}
		o.cleanupLogStreaming(state)
		return true
	}

	// Check if sidecar is healthy and worker not started → start worker
	if !state.workerStarted && sidecarInspect.State.Health != nil && sidecarInspect.State.Health.Status == "healthy" {
		logger.Info("Sidecar healthy (reconciled), starting worker")
		state.startTime = time.Now()
		var err error
		state.logCancel, state.logDone, err = o.startWorkerAndLogs(ctx, logger, cfg, js)
		if err == nil {
			state.workerStarted = true
		}
	}

	// Resume log streaming for already-started workers (e.g. after service restart)
	if state.workerStarted && state.logCancel == nil && !state.workerExited {
		logCtx, logCancel := context.WithCancel(ctx)
		logDone := make(chan struct{})
		go func() {
			defer close(logDone)
			o.streamLogs(logCtx, logger, cfg.jobID, js.jobContainerID, cfg.dest)
		}()
		state.logCancel = logCancel
		state.logDone = logDone
	}

	// Check worker state if started
	if state.workerStarted && !state.workerExited {
		workerInspect, err := o.client.ContainerInspect(ctx, js.jobContainerID)
		if err != nil {
			logger.Warn("Failed to inspect worker during reconcile", "error", err)
		} else if !workerInspect.State.Running {
			// Worker has exited
			state.workerExited = true
			exitCode := workerInspect.State.ExitCode
			logger.Info("Worker exited (reconciled)", "exitCode", exitCode)

			if exitCode == 0 {
				_ = o.store.Set(cfg.jobID, job.StateCompleted, job.WithExitCode(exitCode))
			} else {
				_ = o.store.Set(cfg.jobID, job.StateFailed, job.WithExitCode(exitCode))
			}

			o.cleanupLogStreaming(state)

			var duration float64
			if !state.startTime.IsZero() {
				duration = time.Since(state.startTime).Seconds()
			}
			o.sendExitEvent(cfg.jobID, cfg, exitCode, duration)

			// Signal sidecar
			if err := o.client.ContainerKill(ctx, js.sidecarContainerID, "SIGUSR1"); err != nil {
				logger.Warn("Failed to signal sidecar", "error", err)
			}
		}
	}

	return false
}

// processJobEvents handles the event stream until completion or error.
// Returns true if job is complete, false if reconnection is needed.
func (o *Orchestrator) processJobEvents(ctx context.Context, logger *slog.Logger, cfg *watchConfig, js *jobState, state *jobWatchState, eventCh <-chan events.Message, errCh <-chan error) bool {
	for {
		select {
		case <-ctx.Done():
			o.cleanupLogStreaming(state)
			return true

		case err := <-errCh:
			if err != nil {
				logger.Warn("Event stream error", "error", err)
			}
			return false // Reconnect

		case event, ok := <-eventCh:
			if !ok {
				return false // Channel closed, reconnect
			}

			switch {
			// Sidecar became healthy → start worker
			case event.Actor.ID == js.sidecarContainerID &&
				event.Action == "health_status: healthy" &&
				!state.workerStarted:

				logger.Info("Sidecar healthy, starting worker")
				state.startTime = time.Now()
				var startErr error
				state.logCancel, state.logDone, startErr = o.startWorkerAndLogs(ctx, logger, cfg, js)
				if startErr == nil {
					state.workerStarted = true
				}

			// Worker exited → signal sidecar
			case event.Actor.ID == js.jobContainerID &&
				event.Action == "die" &&
				!state.workerExited:

				state.workerExited = true
				exitCode := o.getExitCode(event)
				logger.Info("Worker exited", "exitCode", exitCode)

				if exitCode == 0 {
					_ = o.store.Set(cfg.jobID, job.StateCompleted, job.WithExitCode(exitCode))
				} else {
					_ = o.store.Set(cfg.jobID, job.StateFailed, job.WithExitCode(exitCode))
				}

				// Give logs a moment to flush
				time.Sleep(500 * time.Millisecond)
				o.cleanupLogStreaming(state)

				// Send exit event
				var duration float64
				if !state.startTime.IsZero() {
					duration = time.Since(state.startTime).Seconds()
				}
				o.sendExitEvent(cfg.jobID, cfg, exitCode, duration)

				// Signal sidecar to process outputs
				if err := o.client.ContainerKill(ctx, js.sidecarContainerID, "SIGUSR1"); err != nil {
					logger.Warn("Failed to signal sidecar", "error", err)
				}

			// Sidecar exited → job complete (or failed if worker never started)
			case event.Actor.ID == js.sidecarContainerID && event.Action == "die":
				switch {
				case !state.workerStarted:
					logger.Error("Sidecar exited before inputs completed")
					_ = o.store.Set(cfg.jobID, job.StateFailed, job.WithExitCode(-1))
					o.sendExitEvent(cfg.jobID, cfg, -1, 0)
				case !state.workerExited:
					logger.Warn("Sidecar exited while worker still running")
				default:
					logger.Info("Sidecar exited, job complete")
				}
				return true
			}
		}
	}
}

// cleanupLogStreaming stops log streaming if active.
func (o *Orchestrator) cleanupLogStreaming(state *jobWatchState) {
	if state.logCancel != nil {
		state.logCancel()
		<-state.logDone
		state.logCancel = nil
		state.logDone = nil
	}
}

// startWorkerAndLogs starts the worker container, emits the start event, and begins log streaming.
// Returns error if worker fails to start.
func (o *Orchestrator) startWorkerAndLogs(ctx context.Context, logger *slog.Logger, cfg *watchConfig, js *jobState) (context.CancelFunc, chan struct{}, error) {
	// Start worker container
	if err := o.client.ContainerStart(ctx, js.jobContainerID, container.StartOptions{}); err != nil {
		logger.Error("Failed to start worker", "error", err)
		// Send failure event
		o.sendExitEvent(cfg.jobID, cfg, -1, 0)
		return nil, nil, err
	}

	_ = o.store.Set(cfg.jobID, job.StateRunning)

	// Emit start event
	if cfg.dest != nil {
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

	// Start log streaming
	logCtx, logCancel := context.WithCancel(ctx)
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		o.streamLogs(logCtx, logger, cfg.jobID, js.jobContainerID, cfg.dest)
	}()

	return logCancel, logDone, nil
}

// getExitCode extracts exit code from a container die event.
func (o *Orchestrator) getExitCode(event events.Message) int {
	if code, ok := event.Actor.Attributes["exitCode"]; ok {
		var exitCode int
		if _, err := fmt.Sscanf(code, "%d", &exitCode); err == nil {
			return exitCode
		}
	}
	return -1
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

func (o *Orchestrator) streamLogs(ctx context.Context, logger *slog.Logger, jobID, containerID string, dest *callbackDest) {
	// Skip log streaming if no callback destination or log events not requested
	if dest == nil || !job.FilteredEvents(job.EventTypeLog, dest.events) {
		o.waitForContainerLogs(ctx, containerID)
		return
	}

	logs, err := o.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
	})
	if err != nil {
		logger.Error("Failed to get container logs", "error", err)
		return
	}
	defer logs.Close()

	builder := job.NewEventBuilder(jobID, "orchestrator/service", dest.meta)
	header := make([]byte, 8)

	for ctx.Err() == nil {
		if _, err := io.ReadFull(logs, header); err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("Log stream ended", "error", err)
			}
			return
		}

		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		if size == 0 {
			continue
		}

		payload := make([]byte, size)
		if _, err := io.ReadFull(logs, payload); err != nil {
			logger.Debug("Failed to read log payload", "error", err)
			return
		}

		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}

		if lines := splitLines(string(payload)); len(lines) > 0 {
			event := builder.BuildLogEvent(lines, stream)
			o.emitter.Emit(&job.Event{
				Payload:     event,
				CallbackURL: dest.url,
				SigningKey:  dest.key,
			})
		}
	}
}

// waitForContainerLogs consumes logs without sending them (for jobs without log callbacks).
func (o *Orchestrator) waitForContainerLogs(ctx context.Context, containerID string) {
	logs, err := o.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return
	}
	defer logs.Close()

	// Consume logs to prevent Docker from buffering
	_, _ = io.Copy(io.Discard, logs)
}

func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
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
