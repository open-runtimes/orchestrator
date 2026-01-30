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
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
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
	client           *client.Client
	sidecarImage     string
	retentionPeriod  time.Duration
	dispatcher       dispatcher.Dispatcher
	callbackProxyURL string
	extraHosts       []string
	metrics          *observability.Metrics
	state            *stateRepo

	cancelMaintenance context.CancelFunc
	watchWg           sync.WaitGroup
}

// Config holds configuration for the Docker orchestrator.
type Config struct {
	SidecarImage        string
	RetentionPeriod     time.Duration          // How long to keep completed jobs (default 15m)
	MaintenanceInterval time.Duration          // How often to run cleanup (default 1m)
	Dispatcher          dispatcher.Dispatcher  // Callback dispatcher (required)
	CallbackProxyURL    string                 // Internal URL for sidecar callbacks (e.g., http://host.docker.internal:8080)
	ExtraHosts          []string               // Extra /etc/hosts entries for containers (e.g., ["appwrite.test:host-gateway"])
	Metrics             *observability.Metrics // Metrics recorder (optional)
}

// NewOrchestrator creates a new Docker orchestrator.
// It automatically reconciles any jobs that were running before a restart.
func NewOrchestrator(ctx context.Context, cfg Config) (*Orchestrator, error) {
	if cfg.Dispatcher == nil {
		return nil, fmt.Errorf("dispatcher is required")
	}

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

	o := &Orchestrator{
		client:           dockerClient,
		sidecarImage:     cfg.SidecarImage,
		retentionPeriod:  retentionPeriod,
		dispatcher:       cfg.Dispatcher,
		callbackProxyURL: cfg.CallbackProxyURL,
		extraHosts:       cfg.ExtraHosts,
		metrics:          cfg.Metrics,
		state:            newStateRepo(),
	}

	if err := o.reconcile(ctx); err != nil {
		slog.Warn("Failed to reconcile jobs", "error", err)
	}

	// Start background maintenance
	maintenanceCtx, cancel := context.WithCancel(context.Background())
	o.cancelMaintenance = cancel
	go o.runMaintenance(maintenanceCtx, maintenanceInterval)

	return o, nil
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

		case sidecarRunning && !workerCreated:
			// Sidecar running, worker not created - shouldn't happen in normal flow
			logger.Warn("Job has sidecar but no worker container", "jobId", jobID)

		case sidecarRunning && workerCreated && !workerRunning:
			// Worker was created but hasn't started or already exited
			// Check if worker has run (has exit code) vs never started
			if jc.worker.State == "created" {
				// Worker never started - check if sidecar is healthy
				if o.isSidecarHealthy(ctx, js.sidecarContainerID) {
					// Start worker now
					resumed++
					callbackCfg := o.extractCallbackConfig(ctx, jc.sidecar)
					o.resumeFromHealthy(ctx, jobID, js, callbackCfg)
				} else {
					// Wait for sidecar to become healthy
					resumed++
					callbackCfg := o.extractCallbackConfig(ctx, jc.sidecar)
					o.resumeFromStart(ctx, jobID, js, callbackCfg)
				}
			} else {
				// Worker exited but sidecar still running - signal it
				resumed++
				logger.Info("Worker exited, signaling sidecar", "jobId", jobID)
				if err := o.client.ContainerKill(ctx, js.sidecarContainerID, "SIGUSR1"); err != nil {
					logger.Warn("Failed to signal sidecar", "error", err, "jobId", jobID)
				}
			}

		case workerRunning:
			// Worker is running - resume watching
			resumed++
			callbackCfg := o.extractCallbackConfig(ctx, jc.sidecar)
			o.resumeFromRunning(ctx, jobID, js, callbackCfg)
		}
	}

	logger.Info("Reconciliation complete", "reconciled", reconciled, "resumed", resumed, "completed", completed)
	return nil
}

// resumeFromStart resumes watching a job where sidecar is still processing inputs.
func (o *Orchestrator) resumeFromStart(ctx context.Context, jobID string, js *jobState, callbackCfg *callbackConfig) {
	logger := slog.With("jobId", jobID, "resumed", true, "phase", "pre-artifacts")

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	js.cancelWatch = cancelWatch
	o.watchWg.Add(1)
	go func() {
		defer o.watchWg.Done()
		o.watchJobEventsResume(watchCtx, logger, jobID, js, callbackCfg, false)
	}()
}

// resumeFromHealthy resumes watching a job where sidecar is healthy but worker hasn't started.
func (o *Orchestrator) resumeFromHealthy(ctx context.Context, jobID string, js *jobState, callbackCfg *callbackConfig) {
	logger := slog.With("jobId", jobID, "resumed", true, "phase", "healthy")

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	js.cancelWatch = cancelWatch
	o.watchWg.Add(1)
	go func() {
		defer o.watchWg.Done()
		o.watchJobEventsResume(watchCtx, logger, jobID, js, callbackCfg, true)
	}()
}

// resumeFromRunning resumes watching a job where worker is already running.
func (o *Orchestrator) resumeFromRunning(ctx context.Context, jobID string, js *jobState, callbackCfg *callbackConfig) {
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	js.cancelWatch = cancelWatch
	o.watchWg.Add(1)
	go func() {
		defer o.watchWg.Done()
		o.resumeWatch(watchCtx, jobID, js, callbackCfg)
	}()
}

// watchJobEventsResume is like watchJobEvents but for resumed jobs.
func (o *Orchestrator) watchJobEventsResume(ctx context.Context, logger *slog.Logger, jobID string, js *jobState, callbackCfg *callbackConfig, sidecarHealthy bool) {
	// Build callback destination
	var dest *callbackDest
	if callbackCfg != nil && callbackCfg.url != "" {
		dest = &callbackDest{
			jobID:  jobID,
			meta:   callbackCfg.meta,
			url:    callbackCfg.url,
			key:    callbackCfg.key,
			events: callbackCfg.events,
		}
	}

	// Subscribe to events for both containers
	eventFilter := filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("container", js.sidecarContainerID),
		filters.Arg("container", js.jobContainerID),
	)
	eventCh, errCh := o.client.Events(ctx, events.ListOptions{Filters: eventFilter})

	// Track state
	workerStarted := sidecarHealthy
	workerExited := false
	var logCancel context.CancelFunc
	var logDone chan struct{}

	// If sidecar is already healthy, start worker
	if sidecarHealthy {
		logger.Info("Resuming: starting worker")
		if err := o.client.ContainerStart(ctx, js.jobContainerID, container.StartOptions{}); err != nil {
			logger.Error("Failed to start worker", "error", err)
			return
		}
		// Don't send start event for resumed jobs
		logCtx, lc := context.WithCancel(ctx)
		logCancel = lc
		logDone = make(chan struct{})
		go func() {
			defer close(logDone)
			o.streamLogs(logCtx, logger, jobID, js.jobContainerID, dest)
		}()
	}

	for {
		select {
		case <-ctx.Done():
			if logCancel != nil {
				logCancel()
				<-logDone
			}
			return

		case err := <-errCh:
			logger.Error("Event stream error", "error", err)
			if logCancel != nil {
				logCancel()
				<-logDone
			}
			return

		case event := <-eventCh:
			switch {
			// Sidecar became healthy → start worker
			case event.Actor.ID == js.sidecarContainerID &&
				event.Action == "health_status: healthy" &&
				!workerStarted:

				logger.Info("Sidecar healthy, starting worker")
				workerStarted = true
				if err := o.client.ContainerStart(ctx, js.jobContainerID, container.StartOptions{}); err != nil {
					logger.Error("Failed to start worker", "error", err)
					return
				}
				// Don't send start event for resumed jobs
				logCtx, lc := context.WithCancel(ctx)
				logCancel = lc
				logDone = make(chan struct{})
				go func() {
					defer close(logDone)
					o.streamLogs(logCtx, logger, jobID, js.jobContainerID, dest)
				}()

			// Worker exited → signal sidecar
			case event.Actor.ID == js.jobContainerID &&
				event.Action == "die" &&
				!workerExited:

				workerExited = true
				exitCode := o.getExitCode(event)
				logger.Info("Worker exited", "exitCode", exitCode)

				// Give logs a moment to flush
				time.Sleep(500 * time.Millisecond)
				if logCancel != nil {
					logCancel()
					<-logDone
				}

				// Don't send exit event for resumed jobs (we might have already sent it)

				// Signal sidecar to process outputs
				if err := o.client.ContainerKill(ctx, js.sidecarContainerID, "SIGUSR1"); err != nil {
					logger.Warn("Failed to signal sidecar", "error", err)
				}

			// Sidecar exited → job complete (or failed if worker never started)
			case event.Actor.ID == js.sidecarContainerID && event.Action == "die":
				switch {
				case !workerStarted:
					logger.Error("Sidecar exited before worker started (resumed job)")
				case !workerExited:
					logger.Warn("Sidecar exited while worker still running (resumed job)")
				default:
					logger.Info("Sidecar exited, job complete")
				}
				return
			}
		}
	}
}

// callbackConfig holds callback configuration extracted from a container.
type callbackConfig struct {
	meta   map[string]string
	url    string
	key    string
	events []string
}

// callbackDest holds destination info for dispatching events.
type callbackDest struct {
	jobID  string
	meta   map[string]string
	url    string
	key    string
	events []string
}

// extractCallbackConfig extracts callback configuration from a sidecar container's environment.
func (o *Orchestrator) extractCallbackConfig(ctx context.Context, sidecar *container.Summary) *callbackConfig {
	if sidecar == nil {
		return nil
	}

	inspect, err := o.client.ContainerInspect(ctx, sidecar.ID)
	if err != nil {
		return nil
	}

	cfg := &callbackConfig{}
	for _, env := range inspect.Config.Env {
		key, value, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		switch key {
		case "CALLBACK_URL":
			cfg.url = value
		case "CALLBACK_KEY":
			cfg.key = value
		case "CALLBACK_EVENTS":
			if value != "" {
				cfg.events = strings.Split(value, ",")
			}
		case "JOB_META":
			if value != "" {
				_ = json.Unmarshal([]byte(value), &cfg.meta)
			}
		}
	}

	if cfg.url == "" {
		return nil
	}
	return cfg
}

// resumeWatch watches a recovered job without sending the start event.
// Metrics are not recorded for resumed jobs since they weren't counted as "created" by this instance.
func (o *Orchestrator) resumeWatch(ctx context.Context, jobID string, js *jobState, callbackCfg *callbackConfig) {
	logger := slog.With("jobId", jobID, "resumed", true)

	// Build callback destination from config
	var dest *callbackDest
	if callbackCfg != nil && callbackCfg.url != "" {
		dest = &callbackDest{
			jobID:  jobID,
			meta:   callbackCfg.meta,
			url:    callbackCfg.url,
			key:    callbackCfg.key,
			events: callbackCfg.events,
		}
	}

	// Pass zero time to skip metrics for resumed jobs
	o.watchUntilExit(ctx, logger, jobID, "", js.jobContainerID, dest, time.Time{})

	// Signal sidecar that worker has exited so it can process outputs
	if js.sidecarContainerID != "" {
		if err := o.client.ContainerKill(ctx, js.sidecarContainerID, "SIGUSR1"); err != nil {
			logger.Warn("Failed to signal sidecar", "error", err)
		}
	}
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

	js := &jobState{
		volumeName: fmt.Sprintf("job-%s-workspace", req.ID),
	}

	// On failure, clean up resources and release reservation
	success := false
	defer func() {
		if !success {
			o.cleanup(ctx, js)
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
	o.watchWg.Add(1)
	go func() {
		defer o.watchWg.Done()
		o.watchJobEvents(watchCtx, req, js)
	}()

	return nil
}

// Stop stops a running job and cleans up its resources.
func (o *Orchestrator) Stop(ctx context.Context, jobID string) error {
	js, exists := o.state.release(jobID)
	if !exists {
		return apperrors.NotFound("job", jobID)
	}

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
	js, exists := o.state.get(jobID)
	if !exists {
		return nil, apperrors.NotFound("job", jobID)
	}

	// Job is reserved but still initializing
	if js == nil {
		return &job.Status{ID: jobID, State: job.StateAccepted}, nil
	}

	status := &job.Status{ID: jobID}

	// Check worker container state
	inspect, err := o.client.ContainerInspect(ctx, js.jobContainerID)
	if err != nil {
		return nil, apperrors.Internal("docker.inspectContainer", err)
	}

	switch {
	case inspect.State.Running:
		status.State = job.StateRunning

	case inspect.State.Status == "created":
		// Worker created but not started - still waiting for sidecar inputs
		status.State = job.StateAccepted

	default:
		// Container has exited - set exit code and determine state
		exitCode := inspect.State.ExitCode
		status.ExitCode = &exitCode

		if exitCode == 0 {
			status.State = job.StateCompleted
		} else {
			status.State = job.StateFailed
			if inspect.State.Error != "" {
				status.Error = inspect.State.Error
			}
		}
	}

	return status, nil
}

// List returns the status of all jobs.
func (o *Orchestrator) List(ctx context.Context) ([]job.Status, error) {
	jobIDs := o.state.ids()
	statuses := make([]job.Status, 0, len(jobIDs))
	for _, id := range jobIDs {
		status, err := o.Status(ctx, id)
		if err != nil {
			continue
		}
		statuses = append(statuses, *status)
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
// The watcher reconnects on event stream errors after reconciling current state.
func (o *Orchestrator) watchJobEvents(ctx context.Context, req *job.Request, js *jobState) {
	logger := slog.With("jobId", req.ID)

	// Build callback destination
	var dest *callbackDest
	if req.Callback != nil && req.Callback.URL != "" {
		dest = &callbackDest{
			jobID:  req.ID,
			meta:   req.Meta,
			url:    req.Callback.URL,
			key:    req.Callback.Key,
			events: req.Callback.Events,
		}
	}

	state := &jobWatchState{}

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
		done := o.reconcileJobState(ctx, logger, req, js, dest, state)
		if done {
			return
		}

		// Process events until error or completion
		done = o.processJobEvents(ctx, logger, req, js, dest, state, eventCh, errCh)
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
func (o *Orchestrator) reconcileJobState(ctx context.Context, logger *slog.Logger, req *job.Request, js *jobState, dest *callbackDest, state *jobWatchState) bool {
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
			o.sendExitEvent(logger, req.ID, dest, -1)
			if o.metrics != nil {
				o.metrics.RecordJobCompleted(context.Background(), req.Image, false, 0)
			}
		}
		o.cleanupLogStreaming(state)
		return true
	}

	// Check if sidecar is healthy and worker not started
	if !state.workerStarted && sidecarInspect.State.Health != nil && sidecarInspect.State.Health.Status == "healthy" {
		logger.Info("Sidecar healthy (reconciled), starting worker")
		state.startTime = time.Now()
		var err error
		state.logCancel, state.logDone, err = o.startWorkerAndLogs(ctx, logger, req, js, dest)
		if err == nil {
			state.workerStarted = true
		}
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

			o.cleanupLogStreaming(state)
			o.sendExitEvent(logger, req.ID, dest, exitCode)

			if o.metrics != nil && !state.startTime.IsZero() {
				duration := time.Since(state.startTime).Seconds()
				o.metrics.RecordJobCompleted(context.Background(), req.Image, exitCode == 0, duration)
			}

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
func (o *Orchestrator) processJobEvents(ctx context.Context, logger *slog.Logger, req *job.Request, js *jobState, dest *callbackDest, state *jobWatchState, eventCh <-chan events.Message, errCh <-chan error) bool {
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
				state.logCancel, state.logDone, startErr = o.startWorkerAndLogs(ctx, logger, req, js, dest)
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

				// Give logs a moment to flush
				time.Sleep(500 * time.Millisecond)
				o.cleanupLogStreaming(state)

				// Send exit event
				o.sendExitEvent(logger, req.ID, dest, exitCode)

				// Record metrics
				if o.metrics != nil && !state.startTime.IsZero() {
					duration := time.Since(state.startTime).Seconds()
					o.metrics.RecordJobCompleted(context.Background(), req.Image, exitCode == 0, duration)
				}

				// Signal sidecar to process outputs
				if err := o.client.ContainerKill(ctx, js.sidecarContainerID, "SIGUSR1"); err != nil {
					logger.Warn("Failed to signal sidecar", "error", err)
				}

			// Sidecar exited → job complete (or failed if worker never started)
			case event.Actor.ID == js.sidecarContainerID && event.Action == "die":
				switch {
				case !state.workerStarted:
					logger.Error("Sidecar exited before inputs completed")
					o.sendExitEvent(logger, req.ID, dest, -1)
					if o.metrics != nil {
						o.metrics.RecordJobCompleted(context.Background(), req.Image, false, 0)
					}
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

// isSidecarHealthy checks if the sidecar container is already healthy.
func (o *Orchestrator) isSidecarHealthy(ctx context.Context, containerID string) bool {
	inspect, err := o.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return false
	}
	return inspect.State.Health != nil && inspect.State.Health.Status == "healthy"
}

// startWorkerAndLogs starts the worker container and log streaming.
// Returns error if worker fails to start.
func (o *Orchestrator) startWorkerAndLogs(ctx context.Context, logger *slog.Logger, req *job.Request, js *jobState, dest *callbackDest) (context.CancelFunc, chan struct{}, error) {
	// Start worker container
	if err := o.client.ContainerStart(ctx, js.jobContainerID, container.StartOptions{}); err != nil {
		logger.Error("Failed to start worker", "error", err)
		// Send failure event
		o.sendExitEvent(logger, req.ID, dest, -1)
		return nil, nil, err
	}

	// Send start event
	if dest != nil {
		builder := job.NewEventBuilder(req.ID, "orchestrator/service", dest.meta)
		event := builder.BuildStartEvent()
		if job.FilteredEvents(event.Type, dest.events) {
			if err := o.dispatcher.Dispatch(&dispatcher.Event{
				Payload:     event,
				Destination: dest.url,
				SigningKey:  dest.key,
			}); err != nil {
				logger.Warn("Failed to dispatch start event", "error", err)
			}
		}
	}

	// Start log streaming
	logCtx, logCancel := context.WithCancel(ctx)
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		o.streamLogs(logCtx, logger, req.ID, js.jobContainerID, dest)
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

// sendExitEvent dispatches the job exit event.
func (o *Orchestrator) sendExitEvent(logger *slog.Logger, jobID string, dest *callbackDest, exitCode int) {
	if dest == nil {
		return
	}
	builder := job.NewEventBuilder(jobID, "orchestrator/service", dest.meta)
	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("exit code %d", exitCode)
	}
	event := builder.BuildExitEvent(exitCode, exitErr)
	if job.FilteredEvents(event.Type, dest.events) {
		if err := o.dispatcher.Dispatch(&dispatcher.Event{
			Payload:     event,
			Destination: dest.url,
			SigningKey:  dest.key,
		}); err != nil {
			logger.Warn("Failed to dispatch exit event", "error", err)
		}
	}
}

// watchUntilExit streams logs and waits for a container to exit, then sends the exit callback.
func (o *Orchestrator) watchUntilExit(ctx context.Context, logger *slog.Logger, jobID, image, containerID string, dest *callbackDest, startTime time.Time) {
	// Start log streaming in background
	logCtx, logCancel := context.WithCancel(ctx)
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		o.streamLogs(logCtx, logger, jobID, containerID, dest)
	}()

	// Wait for container to exit
	exitCode, exitErr := o.waitForExit(ctx, containerID)
	logger = logger.With("exitCode", exitCode)
	if exitErr != nil {
		logger = logger.With("exitError", exitErr)
	}

	// Give logs a moment to flush
	time.Sleep(500 * time.Millisecond)
	logCancel()
	<-logDone

	// Send exit event
	if dest != nil {
		builder := job.NewEventBuilder(jobID, "orchestrator/service", dest.meta)
		event := builder.BuildExitEvent(exitCode, exitErr)
		if job.FilteredEvents(event.Type, dest.events) {
			if err := o.dispatcher.Dispatch(&dispatcher.Event{
				Payload:     event,
				Destination: dest.url,
				SigningKey:  dest.key,
			}); err != nil {
				logger.Warn("Failed to dispatch exit event", "error", err)
			}
		}
	}

	logger.Info("Job completed")

	// Record job completion metrics (skip for resumed jobs where startTime is zero)
	if o.metrics != nil && !startTime.IsZero() {
		duration := time.Since(startTime).Seconds()
		success := exitCode == 0 && exitErr == nil
		o.metrics.RecordJobCompleted(context.Background(), image, success, duration)
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
			// Ignore dispatch errors - dispatcher logs drops internally
			_ = o.dispatcher.Dispatch(&dispatcher.Event{
				Payload:     event,
				Destination: dest.url,
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

func (o *Orchestrator) waitForExit(ctx context.Context, containerID string) (int, error) {
	statusCh, errCh := o.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case err := <-errCh:
		return -1, err
	case status := <-statusCh:
		if status.Error != nil {
			return int(status.StatusCode), fmt.Errorf("%s", status.Error.Message)
		}
		return int(status.StatusCode), nil
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

	containerConfig := &container.Config{
		Image:      req.Image,
		Cmd:        cmd,
		Env:        env,
		WorkingDir: req.Workspace,
		Labels: map[string]string{
			"job.id":     req.ID,
			"job.type":   "worker",
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

// cleanupExpiredJobs removes jobs that completed more than retentionPeriod ago.
func (o *Orchestrator) cleanupExpiredJobs(ctx context.Context) {
	now := time.Now()
	logger := slog.With("component", "maintenance")

	// Collect job IDs and container IDs to check
	type jobInfo struct {
		id          string
		containerID string
	}
	var toCheck []jobInfo

	for jobID, js := range o.state.list() {
		if js == nil {
			continue
		}
		toCheck = append(toCheck, jobInfo{id: jobID, containerID: js.jobContainerID})
	}

	// Check each container's state via Docker
	var expired []string
	for _, j := range toCheck {
		inspect, err := o.client.ContainerInspect(ctx, j.containerID)
		if err != nil {
			expired = append(expired, j.id)
			continue
		}

		if inspect.State.Running {
			continue
		}

		finishedAt, err := time.Parse(time.RFC3339Nano, inspect.State.FinishedAt)
		if err != nil {
			continue
		}
		if now.Sub(finishedAt) > o.retentionPeriod {
			expired = append(expired, j.id)
		}
	}

	if len(expired) == 0 {
		return
	}

	for _, jobID := range expired {
		if js, exists := o.state.release(jobID); exists && js != nil {
			o.cleanup(ctx, js)
			logger.Debug("Cleaned up expired job", "jobId", jobID)
		}
	}

	logger.Info("Maintenance complete", "cleaned", len(expired))
}

// Verify Orchestrator implements job.Orchestrator
var _ job.Orchestrator = (*Orchestrator)(nil)
