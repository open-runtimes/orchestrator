// Package docker implements the job.Orchestrator interface using the Docker API.
// Jobs run directly on the host Docker daemon.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/job"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// dockerHandle carries the Docker infrastructure identifiers for a running job.
type dockerHandle struct {
	sidecarContainerID string
	jobContainerID     string
	volumeName         string
}

// Orchestrator implements job.Orchestrator using Docker.
type Orchestrator struct {
	client              *client.Client
	sidecarImage        string
	retentionPeriod     time.Duration
	maintenanceInterval time.Duration
	emitter             *job.CallbackEmitter
	artifactEndpoint    string
	extraHosts          []string
	networkName         string
	ctrl                *job.MemoryStore[dockerHandle]
	watcher             LifecycleWatcher

	cancelMaintenance context.CancelFunc
	watchWg           sync.WaitGroup
}

// Config holds configuration for the Docker orchestrator.
type Config struct {
	SidecarImage        string
	RetentionPeriod     time.Duration // How long to keep completed jobs (default 15m)
	MaintenanceInterval time.Duration // How often to run cleanup (default 1m)
	ArtifactEndpoint    string        // Base URL for sidecar artifact reporting (e.g., http://host.docker.internal:8080)
	ExtraHosts          []string      // Extra /etc/hosts entries for containers (e.g., ["appwrite.test:host-gateway"])
	Network             string        // Docker network to attach worker and sidecar containers to
}

// NewOrchestrator returns an OrchestratorFactory that creates a Docker orchestrator.
// Register listeners on the emitter before calling Start.
func NewOrchestrator(ctx context.Context, cfg Config) job.OrchestratorFactory {
	return func(emitter *job.CallbackEmitter) (job.Orchestrator, error) {
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
			artifactEndpoint:    cfg.ArtifactEndpoint,
			extraHosts:          cfg.ExtraHosts,
			networkName:         cfg.Network,
			ctrl:                job.NewMemoryStore[dockerHandle](),
			watcher:             newDockerLifecycleWatcher(dockerClient),
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
			// Both exited — replay terminal state, no watcher needed.
			completed++
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			_ = o.ctrl.Reserve(jobID)
			o.ctrl.Commit(jobID, handle, nil)
			_ = o.ctrl.Apply(jobID, job.Started{})
			_ = o.ctrl.Apply(jobID, job.Exited{ExitCode: cs.workerExitCode})

		case sidecarRunning && jc.worker == nil:
			// Sidecar running but no worker — shouldn't happen in normal flow.
			logger.Warn("Job has sidecar but no worker container", "jobId", jobID)
			resumed++
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			cfg := watchConfigFromState(cs, handle)
			_ = o.ctrl.Reserve(jobID)
			o.ctrl.Commit(jobID, handle, cancelWatch)
			_ = o.ctrl.Apply(jobID, job.Started{})
			o.watchWg.Go(func() {
				o.watcher.Watch(watchCtx, cfg.sidecarID, cfg.workerID, func(s job.Signal) {
					_ = o.ctrl.Apply(cfg.jobID, s)
					job.EmitCallback(o.emitter, cfg.jobID, cfg.image, cfg.dest, s)
				})
			})

		case workerRunning:
			// Worker is running — replay running state and resume watcher.
			resumed++
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			cfg := watchConfigFromState(cs, handle)
			_ = o.ctrl.Reserve(jobID)
			o.ctrl.Commit(jobID, handle, cancelWatch)
			_ = o.ctrl.Apply(jobID, job.Started{})
			o.watchWg.Go(func() {
				o.watcher.Watch(watchCtx, cfg.sidecarID, cfg.workerID, func(s job.Signal) {
					_ = o.ctrl.Apply(cfg.jobID, s)
					job.EmitCallback(o.emitter, cfg.jobID, cfg.image, cfg.dest, s)
				})
			})

		default:
			// Sidecar running, worker created but not yet started — accepted state.
			// The watcher will drive the Running transition when the worker starts.
			resumed++
			watchCtx, cancelWatch := context.WithCancel(context.Background())
			cs := inspectContainers(ctx, o.client, jobID, handle.jobContainerID)
			cfg := watchConfigFromState(cs, handle)
			_ = o.ctrl.Reserve(jobID)
			o.ctrl.Commit(jobID, handle, cancelWatch)
			o.watchWg.Go(func() {
				o.watcher.Watch(watchCtx, cfg.sidecarID, cfg.workerID, func(s job.Signal) {
					_ = o.ctrl.Apply(cfg.jobID, s)
					job.EmitCallback(o.emitter, cfg.jobID, cfg.image, cfg.dest, s)
				})
			})
		}
	}

	logger.Info("Reconciliation complete", "reconciled", reconciled, "resumed", resumed, "completed", completed)
	return nil
}

// Run creates and starts a job with its sidecar.
func (o *Orchestrator) Run(ctx context.Context, req *job.Request) error {
	if err := o.ctrl.Reserve(req.ID); err != nil {
		return err
	}

	h := dockerHandle{
		volumeName: fmt.Sprintf("job-%s-workspace", req.ID),
	}

	// On failure, release the reservation and clean up any created resources.
	success := false
	defer func() {
		if !success {
			if rh, ok := o.ctrl.Release(req.ID); ok {
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

	if err := o.pullImageIfNeeded(pullCtx, o.sidecarImage); err != nil {
		return apperrors.Internal("docker.pullSidecarImage", err)
	}

	// Create sidecar container
	if h.sidecarContainerID, err = o.createSidecarContainer(ctx, req, h); err != nil {
		return apperrors.Internal("docker.createSidecarContainer", err)
	}

	// Start sidecar (will process inputs and write marker file)
	if err := o.client.ContainerStart(ctx, h.sidecarContainerID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startSidecarContainer", err)
	}

	cfg := watchConfigFromRequest(req, h)

	// Commit runtime handles and start event-driven watcher.
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	o.ctrl.Commit(req.ID, h, cancelWatch)
	success = true
	o.watchWg.Go(func() {
		o.watcher.Watch(watchCtx, cfg.sidecarID, cfg.workerID, func(s job.Signal) {
			_ = o.ctrl.Apply(cfg.jobID, s)
			job.EmitCallback(o.emitter, cfg.jobID, cfg.image, cfg.dest, s)
		})
	})

	return nil
}

// Stop stops a running job and cleans up its resources.
func (o *Orchestrator) Stop(ctx context.Context, jobID string) error {
	h, ok := o.ctrl.Release(jobID)
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
func (o *Orchestrator) Status(ctx context.Context, jobID string) (*job.StatusResponse, error) {
	entry, exists := o.ctrl.Get(jobID)
	if !exists {
		return nil, apperrors.NotFound("job", jobID)
	}
	return entry.StatusResponse(), nil
}

// List returns the status of all jobs.
func (o *Orchestrator) List(ctx context.Context) ([]job.StatusResponse, error) {
	entries := o.ctrl.List()
	statuses := make([]job.StatusResponse, len(entries))
	for i, e := range entries {
		statuses[i] = *e.StatusResponse()
	}
	return statuses, nil
}

// Close releases resources held by the orchestrator.
func (o *Orchestrator) Close() error {
	if o.cancelMaintenance != nil {
		o.cancelMaintenance()
	}

	// Cancel all watch goroutines and wait for them to finish.
	o.ctrl.Each(func(_ string, _ job.Entry, h job.Handle[dockerHandle]) {
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
		if len(req.Callback.Headers) > 0 {
			if headersJSON, err := json.Marshal(req.Callback.Headers); err == nil {
				labels["job.callback.headers"] = string(headersJSON)
			}
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
	networkConfig := o.networkingConfig()

	containerName := fmt.Sprintf("job-%s-worker", req.ID)
	resp, err := o.client.ContainerCreate(ctx, containerConfig, hostConfig, networkConfig, nil, containerName)
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (o *Orchestrator) createSidecarContainer(ctx context.Context, req *job.Request, h dockerHandle) (string, error) {
	env := []string{
		"JOB_ID=" + req.ID,
		fmt.Sprintf("TIMEOUT_SECONDS=%d", req.TimeoutSeconds),
		"SHARED_VOLUME_PATH=" + req.Workspace,
	}

	artifactsJSON, err := artifact.MarshalArtifacts(req.Artifacts)
	if err != nil {
		return "", fmt.Errorf("failed to marshal artifacts: %w", err)
	}
	env = append(env, "ARTIFACTS_JSON="+string(artifactsJSON))

	if o.artifactEndpoint != "" {
		env = append(env, "ARTIFACT_ENDPOINT="+o.artifactEndpoint)
	}

	if req.Callback != nil && req.Callback.URL != "" {
		env = append(env, "CALLBACK_URL="+req.Callback.URL)
		if req.Callback.Key != "" {
			env = append(env, "CALLBACK_KEY="+req.Callback.Key)
		}
		if len(req.Callback.Events) > 0 {
			env = append(env, "CALLBACK_EVENTS="+strings.Join(req.Callback.Events, ","))
		}
		if len(req.Callback.Headers) > 0 {
			headersJSON, err := json.Marshal(req.Callback.Headers)
			if err != nil {
				return "", fmt.Errorf("failed to marshal callback headers: %w", err)
			}
			env = append(env, "CALLBACK_HEADERS_JSON="+string(headersJSON))
		}
	}

	if len(req.Meta) > 0 {
		metaJSON, err := json.Marshal(req.Meta)
		if err != nil {
			return "", fmt.Errorf("failed to marshal meta: %w", err)
		}
		env = append(env, "JOB_META="+string(metaJSON))
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
	networkConfig := o.networkingConfig()

	containerName := fmt.Sprintf("job-%s-sidecar", req.ID)
	resp, err := o.client.ContainerCreate(ctx, containerConfig, hostConfig, networkConfig, nil, containerName)
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (o *Orchestrator) networkingConfig() *network.NetworkingConfig {
	if o.networkName == "" {
		return nil
	}

	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			o.networkName: {},
		},
	}
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
	o.ctrl.Each(func(jobID string, e job.Entry, _ job.Handle[dockerHandle]) {
		if isTerminal(e.State) && now.Sub(e.UpdatedAt) > o.retentionPeriod {
			expired = append(expired, jobID)
		}
	})

	if len(expired) == 0 {
		return
	}

	for _, jobID := range expired {
		if h, ok := o.ctrl.Release(jobID); ok {
			if h.CancelWatch != nil {
				h.CancelWatch()
			}
			o.cleanup(ctx, h.Runtime)
		}
		logger.Debug("Cleaned up expired job", "jobId", jobID)
	}

	logger.Info("Maintenance complete", "cleaned", len(expired))
}

// EmitArtifactEvent receives an artifact result from the sidecar and dispatches
// the corresponding CloudEvent through the orchestrator's delivery pipeline.
// It is a no-op if the job has no callback configured or has already been released.
func (o *Orchestrator) EmitArtifactEvent(r job.ArtifactReport) {
	if r.CallbackURL == "" || !job.MatchesCallbackFilter(job.CallbackTypeArtifact, r.CallbackEvents) {
		return
	}
	builder := job.NewEventBuilder(r.JobID, "orchestrator/service", r.Meta)
	var errVal error
	if r.FailureReason != "" {
		errVal = fmt.Errorf("%s", r.FailureReason)
	}
	event := builder.BuildArtifactEvent(r.ID, r.Type, r.Status, r.Content, errVal)
	o.emitter.Emit(&job.CallbackEnvelope{
		Payload:     event,
		CallbackURL: r.CallbackURL,
		SigningKey:  r.CallbackKey,
		Headers:     r.CallbackHeaders,
	})
}

// Verify Orchestrator implements job.Orchestrator
var _ job.Orchestrator = (*Orchestrator)(nil)
