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
	emitter             *job.EventEmitter
	callbackProxyURL    string
	extraHosts          []string
	registry            *dockerRegistry
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
// Register listeners on the emitter before calling Start.
func NewOrchestrator(ctx context.Context, cfg Config) job.OrchestratorFactory {
	return func(emitter *job.EventEmitter) (job.Orchestrator, error) {
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
			emitter:             emitter,
			callbackProxyURL:    cfg.CallbackProxyURL,
			extraHosts:          cfg.ExtraHosts,
			registry:            newDockerRegistry(),
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
func (o *Orchestrator) reconcile(ctx context.Context) error {
	logger := slog.With("component", "reconcile")

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

	var reconciled, resumed, completed int
	for jobID, jc := range jobs {
		if jc.sidecar == nil {
			logger.Warn("Job missing sidecar container", "jobId", jobID)
			continue
		}

		handle := dockerHandle{
			sidecarContainerID: jc.sidecar.ID,
			volumeName:         fmt.Sprintf("job-%s-workspace", jobID),
		}
		if jc.worker != nil {
			handle.jobContainerID = jc.worker.ID
		}
		reconciled++

		sidecarRunning := jc.sidecar.State == "running"
		workerRunning := jc.worker != nil && jc.worker.State == "running"

		switch {
		case !sidecarRunning && !workerRunning:
			// Both exited — restore with terminal state, no watcher needed.
			completed++
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			exitCode := cs.workerExitCode
			var t job.Transition
			if exitCode == 0 {
				t = job.ToCompleted(exitCode)
			} else {
				t = job.ToFailed(exitCode, "")
			}
			_ = o.registry.Restore(jobID, t, handle, nil)

		case sidecarRunning && jc.worker == nil:
			// Sidecar running but no worker — shouldn't happen in normal flow.
			logger.Warn("Job has sidecar but no worker container", "jobId", jobID)
			resumed++
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			_ = o.registry.Restore(jobID, job.ToRunning(), handle, cancelWatch)
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			cfg := watchConfigFromState(cs, handle)
			o.watchWg.Add(1)
			go func() {
				defer o.watchWg.Done()
				o.runWatchLoop(watchCtx, cfg)
			}()

		case workerRunning:
			// Worker is running — restore as running and resume watcher.
			resumed++
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			_ = o.registry.Restore(jobID, job.ToRunning(), handle, cancelWatch)
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			cfg := watchConfigFromState(cs, handle)
			o.watchWg.Add(1)
			go func() {
				defer o.watchWg.Done()
				o.runWatchLoop(watchCtx, cfg)
			}()

		default:
			// Sidecar running, worker created but not yet running — accepted state.
			// The watcher will drive the Running transition when the worker starts.
			resumed++
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			_ = o.registry.Restore(jobID, job.ToAccepted(), handle, cancelWatch)
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			cfg := watchConfigFromState(cs, handle)
			o.watchWg.Add(1)
			go func() {
				defer o.watchWg.Done()
				o.runWatchLoop(watchCtx, cfg)
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
func (o *Orchestrator) Run(ctx context.Context, req *job.Request) error {
	if err := o.registry.Reserve(req.ID); err != nil {
		return err
	}

	h := dockerHandle{
		volumeName: fmt.Sprintf("job-%s-workspace", req.ID),
	}

	// On failure, release the reservation and clean up any created resources.
	success := false
	defer func() {
		if !success {
			if rh, ok := o.registry.Release(req.ID); ok {
				o.cleanup(ctx, rh.Runtime)
			}
		}
	}()

	// Create shared volume
	if _, err := o.client.VolumeCreate(ctx, volume.CreateOptions{Name: h.volumeName}); err != nil {
		return apperrors.Internal("docker.createVolume", err)
	}

	// Pull job image if needed (with detached context so HTTP timeout doesn't cancel)
	pullCtx := context.WithoutCancel(ctx)
	if err := o.pullImageIfNeeded(pullCtx, req.Image); err != nil {
		return apperrors.Internal("docker.pullImage", err)
	}

	// Create job container (but don't start yet)
	var err error
	if h.jobContainerID, err = o.createJobContainer(ctx, req, h); err != nil {
		return apperrors.Internal("docker.createJobContainer", err)
	}

	// Create sidecar container
	if h.sidecarContainerID, err = o.createSidecarContainer(ctx, req, h); err != nil {
		return apperrors.Internal("docker.createSidecarContainer", err)
	}

	// Start sidecar (will process inputs and write marker file)
	if err := o.client.ContainerStart(ctx, h.sidecarContainerID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startSidecarContainer", err)
	}

	// Commit runtime handles and start event-driven watcher.
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	o.registry.Commit(req.ID, h, cancelWatch)
	success = true

	cfg := watchConfigFromRequest(req, h)
	o.watchWg.Add(1)
	go func() {
		defer o.watchWg.Done()
		o.runWatchLoop(watchCtx, cfg)
	}()

	return nil
}

// runWatchLoop drives job state transitions and callback emission from watcher events.
func (o *Orchestrator) runWatchLoop(ctx context.Context, cfg *watchConfig) {
	for e := range o.watcher.Watch(ctx, cfg.sidecarID, cfg.workerID) {
		switch ev := e.(type) {
		case SidecarReady:
			_ = o.registry.Apply(cfg.jobID, job.ToRunning())
			o.emitStartEvent(cfg)

		case WorkerExited:
			if ev.ExitCode == 0 {
				_ = o.registry.Apply(cfg.jobID, job.ToCompleted(ev.ExitCode))
			} else {
				_ = o.registry.Apply(cfg.jobID, job.ToFailed(ev.ExitCode, ""))
			}
			o.sendExitEvent(cfg.jobID, cfg, ev.ExitCode, ev.Duration.Seconds())

		case SidecarExited:
			if !ev.WorkerEverStarted {
				_ = o.registry.Apply(cfg.jobID, job.ToFailed(-1, "sidecar exited before worker started"))
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
	h, ok := o.registry.Release(jobID)
	if !ok {
		return apperrors.NotFound("job", jobID)
	}

	if h.CancelWatch != nil {
		h.CancelWatch()
	}

	o.cleanup(ctx, h.Runtime)
	return nil
}

// Status returns the current status of a job.
func (o *Orchestrator) Status(ctx context.Context, jobID string) (*job.Status, error) {
	entry, exists := o.registry.Get(jobID)
	if !exists {
		return nil, apperrors.NotFound("job", jobID)
	}
	return entry.Status(), nil
}

// List returns the status of all jobs.
func (o *Orchestrator) List(ctx context.Context) ([]job.Status, error) {
	entries := o.registry.List()
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

	// Cancel all watch goroutines and wait for them to finish.
	o.registry.Each(func(_ string, _ job.Entry, h job.Handle[dockerHandle]) {
		if h.CancelWatch != nil {
			h.CancelWatch()
		}
	})
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

func (o *Orchestrator) createJobContainer(ctx context.Context, req *job.Request, h dockerHandle) (string, error) {
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
				Source: h.volumeName,
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

func (o *Orchestrator) createSidecarContainer(ctx context.Context, req *job.Request, h dockerHandle) (string, error) {
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

	healthCheck := &container.HealthConfig{
		Test:        []string{"CMD", "/ko-app/job-sidecar", "-check-ready"},
		Interval:    200 * time.Millisecond,
		Timeout:     5 * time.Second,
		StartPeriod: time.Duration(req.TimeoutSeconds) * time.Second,
		Retries:     0,
	}

	containerConfig := &container.Config{
		Image:       o.sidecarImage,
		Env:         env,
		User:        "0",
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
				Source: h.volumeName,
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

func (o *Orchestrator) cleanup(ctx context.Context, h dockerHandle) {
	const stopTimeout = 10

	o.removeContainer(ctx, h.sidecarContainerID, stopTimeout)
	o.removeContainer(ctx, h.jobContainerID, stopTimeout)

	if h.volumeName != "" {
		_ = o.client.VolumeRemove(ctx, h.volumeName, true)
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

	var expired []string
	o.registry.Each(func(jobID string, e job.Entry, _ job.Handle[dockerHandle]) {
		if isTerminal(e.State) && now.Sub(e.UpdatedAt) > o.retentionPeriod {
			expired = append(expired, jobID)
		}
	})

	if len(expired) == 0 {
		return
	}

	for _, jobID := range expired {
		if h, ok := o.registry.Release(jobID); ok {
			if h.CancelWatch != nil {
				h.CancelWatch()
			}
			o.cleanup(ctx, h.Runtime)
		}
		logger.Debug("Cleaned up expired job", "jobId", jobID)
	}

	logger.Info("Maintenance complete", "cleaned", len(expired))
}

// Verify Orchestrator implements job.Orchestrator
var _ job.Orchestrator = (*Orchestrator)(nil)
