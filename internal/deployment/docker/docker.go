// Package docker implements the deployment.Orchestrator interface using the
// Docker API. Each deployment is a worker container fronted by a
// deployments-sidecar proxy container, with an optional one-shot artifacts
// step, all sharing a workspace volume. The daemon is the source of truth:
// state derives live from container labels — there is no in-memory store.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Orchestrator implements deployment.Orchestrator using Docker.
type Orchestrator struct {
	client *client.Client
	cfg    Config
}

// NewOrchestrator creates a Docker deployment orchestrator.
func NewOrchestrator(_ context.Context, cfg Config) (*Orchestrator, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return &Orchestrator{client: dockerClient, cfg: cfg}, nil
}

// Start logs a reconcile summary. Deployments are self-sufficient containers —
// there are no watchers to resume, so startup only reports what exists.
func (o *Orchestrator) Start(ctx context.Context) error {
	statuses, err := o.List(ctx)
	if err != nil {
		slog.Warn("Failed to reconcile deployments", "error", err)
		return nil
	}
	slog.Info("Reconciled deployments", "count", len(statuses))
	return nil
}

// Apply creates the deployment or replaces it in place. Applying an identical
// spec is a no-op; any change tears the old containers down and recreates them.
func (o *Orchestrator) Apply(ctx context.Context, req *deployment.Request) error {
	spec, err := json.Marshal(req)
	if err != nil {
		return apperrors.Internal("docker.marshalSpec", err)
	}

	existing, err := o.containersFor(ctx, req.ID)
	if err != nil {
		return apperrors.Internal("docker.listContainers", err)
	}
	if specOf(existing) == string(spec) {
		return nil
	}
	if len(existing) > 0 {
		o.cleanup(ctx, req.ID)
	}

	if err := o.create(ctx, req, string(spec)); err != nil {
		o.cleanup(ctx, req.ID)
		return err
	}
	return nil
}

// create provisions the workspace volume, materializes artifacts, and starts
// the worker and proxy containers.
func (o *Orchestrator) create(ctx context.Context, req *deployment.Request, spec string) error {
	if _, err := o.client.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName(req.ID)}); err != nil {
		return apperrors.Internal("docker.createVolume", err)
	}

	// Detached context so an HTTP request timeout doesn't cancel image pulls.
	pullCtx := context.WithoutCancel(ctx)
	if err := o.pullImageIfNeeded(pullCtx, req.Image); err != nil {
		return apperrors.Internal("docker.pullImage", err)
	}
	if err := o.pullImageIfNeeded(pullCtx, o.cfg.SidecarImage); err != nil {
		return apperrors.Internal("docker.pullSidecarImage", err)
	}

	if len(req.Artifacts) > 0 {
		if err := o.pullImageIfNeeded(pullCtx, o.cfg.ArtifactImage); err != nil {
			return apperrors.Internal("docker.pullArtifactImage", err)
		}
		if err := o.runArtifacts(ctx, req); err != nil {
			return err
		}
	}

	workerID, err := o.startWorker(ctx, req)
	if err != nil {
		return err
	}

	info, err := o.client.ContainerInspect(ctx, workerID)
	if err != nil {
		return apperrors.Internal("docker.inspectWorker", err)
	}
	workerIP := containerIP(info.NetworkSettings, o.cfg.Network)
	if workerIP == "" {
		return apperrors.Internal("docker.workerIP", errors.New("worker container has no IP address"))
	}

	return o.startProxy(ctx, req, workerIP, spec)
}

// runArtifacts materializes req.Artifacts into the workspace volume via a
// one-shot sidecar pre-mode run, waiting for it to finish.
func (o *Orchestrator) runArtifacts(ctx context.Context, req *deployment.Request) error {
	artifactsJSON, err := artifact.MarshalArtifacts(req.Artifacts)
	if err != nil {
		return apperrors.Internal("docker.marshalArtifacts", err)
	}

	env := []string{
		"JOB_ID=dep-" + req.ID,
		"SHARED_VOLUME_PATH=" + workspacePath,
		"ARTIFACTS_JSON=" + string(artifactsJSON),
	}
	if o.cfg.ArtifactEndpoint != "" {
		env = append(env, "ARTIFACT_ENDPOINT="+o.cfg.ArtifactEndpoint)
	}

	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:  o.cfg.ArtifactImage,
			Cmd:    []string{"-mode=pre"},
			Env:    env,
			User:   "0",
			Labels: containerLabels(req.ID, typeArtifacts),
		},
		&container.HostConfig{
			Mounts:     []mount.Mount{o.workspaceMount(req.ID)},
			ExtraHosts: o.cfg.ExtraHosts,
		},
		o.networkingConfig(), nil, artifactsName(req.ID))
	if err != nil {
		return apperrors.Internal("docker.createArtifactsContainer", err)
	}

	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startArtifactsContainer", err)
	}

	exitCode, err := o.waitForExit(ctx, resp.ID)
	if err != nil {
		return apperrors.Internal("docker.waitArtifacts", err)
	}
	o.removeContainer(ctx, resp.ID)
	if exitCode != 0 {
		return apperrors.Internal("docker.artifacts",
			fmt.Errorf("artifacts container exited with code %d", exitCode))
	}
	return nil
}

// startWorker creates and starts the user container, returning its ID.
func (o *Orchestrator) startWorker(ctx context.Context, req *deployment.Request) (string, error) {
	env := make([]string, 0, len(req.Environment))
	for k, v := range req.Environment {
		env = append(env, k+"="+v)
	}

	var cmd []string
	if req.Command != "" {
		cmd = []string{"/bin/sh", "-c", req.Command}
	}

	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:      req.Image,
			Cmd:        cmd,
			Env:        env,
			WorkingDir: workspacePath,
			Labels:     containerLabels(req.ID, typeWorker),
		},
		&container.HostConfig{
			Mounts: []mount.Mount{o.workspaceMount(req.ID)},
			Resources: container.Resources{
				NanoCPUs: int64(req.CPU * 1e9),
				Memory:   int64(req.Memory) * 1024 * 1024,
			},
		},
		o.networkingConfig(), nil, workerName(req.ID))
	if err != nil {
		return "", apperrors.Internal("docker.createWorker", err)
	}
	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", apperrors.Internal("docker.startWorker", err)
	}
	return resp.ID, nil
}

// startProxy creates and starts the deployments-sidecar proxy fronting the
// worker. It carries the canonical spec label that makes Apply declarative.
func (o *Orchestrator) startProxy(ctx context.Context, req *deployment.Request, workerIP, spec string) error {
	labels := containerLabels(req.ID, typeProxy)
	labels[labelSpec] = spec
	labels[labelHost] = req.Host

	healthCheck := &container.HealthConfig{
		Test:          []string{"CMD", "/ko-app/deployments-sidecar", "-check-ready"},
		Interval:      500 * time.Millisecond,
		Timeout:       5 * time.Second,
		StartPeriod:   progressDeadline(req.ProgressDeadlineSeconds),
		StartInterval: 500 * time.Millisecond,
	}

	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:       o.cfg.SidecarImage,
			Env:         proxyEnv(req, workerIP),
			Healthcheck: healthCheck,
			Labels:      labels,
		},
		&container.HostConfig{ExtraHosts: o.cfg.ExtraHosts},
		o.networkingConfig(), nil, proxyName(req.ID))
	if err != nil {
		return apperrors.Internal("docker.createProxy", err)
	}
	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startProxy", err)
	}
	return nil
}

// Delete tears down the deployment's containers and volume.
func (o *Orchestrator) Delete(ctx context.Context, id string) error {
	existing, err := o.containersFor(ctx, id)
	if err != nil {
		return apperrors.Internal("docker.listContainers", err)
	}
	if len(existing) == 0 {
		return apperrors.NotFound("deployment", id)
	}
	o.cleanup(ctx, id)
	return nil
}

// Spec returns the last-applied request, read back from the proxy's label.
func (o *Orchestrator) Spec(ctx context.Context, id string) (*deployment.Request, error) {
	summaries, err := o.containersFor(ctx, id)
	if err != nil {
		return nil, apperrors.Internal("docker.listContainers", err)
	}
	raw := specOf(summaries)
	if raw == "" {
		return nil, apperrors.NotFound("deployment", id)
	}
	req := &deployment.Request{}
	if err := json.Unmarshal([]byte(raw), req); err != nil {
		return nil, apperrors.Internal("docker.unmarshalSpec", err)
	}
	return req, nil
}

// Endpoints returns the proxy endpoint once it is running and healthy. Docker
// runs a single replica, so this is at most one URL.
func (o *Orchestrator) Endpoints(ctx context.Context, id string) ([]*url.URL, error) {
	info, err := o.client.ContainerInspect(ctx, proxyName(id))
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return []*url.URL{}, nil
		}
		return nil, apperrors.Internal("docker.inspectProxy", err)
	}
	if info.State == nil || !info.State.Running ||
		info.State.Health == nil || info.State.Health.Status != container.Healthy {
		return []*url.URL{}, nil
	}
	ip := containerIP(info.NetworkSettings, o.cfg.Network)
	if ip == "" {
		return []*url.URL{}, nil
	}
	return []*url.URL{{
		Scheme: "http",
		Host:   net.JoinHostPort(ip, strconv.Itoa(proxy.DefaultProxyPort)),
	}}, nil
}

// Status returns the deployment's state, derived live from its containers.
func (o *Orchestrator) Status(ctx context.Context, id string) (*deployment.StatusResponse, error) {
	summaries, err := o.containersFor(ctx, id)
	if err != nil {
		return nil, apperrors.Internal("docker.listContainers", err)
	}
	if len(summaries) == 0 {
		return nil, apperrors.NotFound("deployment", id)
	}
	return deriveStatus(id, o.snapshotFor(ctx, summaries), time.Now()), nil
}

// List returns the statuses of all deployments known to the daemon.
func (o *Orchestrator) List(ctx context.Context) ([]deployment.StatusResponse, error) {
	summaries, err := o.listManaged(ctx)
	if err != nil {
		return nil, apperrors.Internal("docker.listContainers", err)
	}

	byID := make(map[string][]container.Summary)
	for _, c := range summaries {
		if id := c.Labels[labelID]; id != "" {
			byID[id] = append(byID[id], c)
		}
	}

	statuses := make([]deployment.StatusResponse, 0, len(byID))
	now := time.Now()
	for id, cs := range byID {
		statuses = append(statuses, *deriveStatus(id, o.snapshotFor(ctx, cs), now))
	}
	return statuses, nil
}

// Ready checks if the Docker daemon is reachable and responsive.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Ping(ctx)
	return err
}

// Close releases the Docker client. Running deployments keep serving.
func (o *Orchestrator) Close() error {
	return o.client.Close()
}

// snapshotFor inspects a deployment's containers and reduces them to a snapshot.
func (o *Orchestrator) snapshotFor(ctx context.Context, summaries []container.Summary) snapshot {
	s := snapshot{deadline: specDeadline(summaries)}

	var workerCreated time.Time
	for _, c := range summaries {
		info, err := o.client.ContainerInspect(ctx, c.ID)
		if err != nil || info.State == nil {
			continue
		}
		switch c.Labels[labelType] {
		case typeWorker:
			s.workerExists = true
			s.workerRunning = info.State.Running
			s.workerExitCode = info.State.ExitCode
			workerCreated = time.Unix(c.Created, 0)
		case typeProxy:
			s.proxyRunning = info.State.Running
			if info.State.Health != nil {
				s.proxyHealth = info.State.Health.Status
			}
			s.created = time.Unix(c.Created, 0)
		}
	}

	if s.created.IsZero() {
		s.created = workerCreated
	}
	return s
}

// listManaged lists all containers carrying the deployments-service label,
// optionally narrowed by extra filters.
func (o *Orchestrator) listManaged(ctx context.Context, extra ...filters.KeyValuePair) ([]container.Summary, error) {
	pairs := append([]filters.KeyValuePair{
		filters.Arg("label", labelManagedBy+"="+managedByValue),
	}, extra...)
	return o.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(pairs...),
	})
}

// containersFor lists all of a deployment's containers.
func (o *Orchestrator) containersFor(ctx context.Context, id string) ([]container.Summary, error) {
	return o.listManaged(ctx, filters.Arg("label", labelID+"="+id))
}

// workspaceMount returns the shared workspace volume mount for a deployment.
func (o *Orchestrator) workspaceMount(id string) mount.Mount {
	return mount.Mount{Type: mount.TypeVolume, Source: volumeName(id), Target: workspacePath}
}

func (o *Orchestrator) networkingConfig() *network.NetworkingConfig {
	if o.cfg.Network == "" {
		return nil
	}
	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			o.cfg.Network: {},
		},
	}
}

func (o *Orchestrator) pullImageIfNeeded(ctx context.Context, imageName string) error {
	if _, err := o.client.ImageInspect(ctx, imageName); err == nil {
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

// waitForExit blocks until the container stops and returns its exit code.
func (o *Orchestrator) waitForExit(ctx context.Context, containerID string) (int64, error) {
	waitCh, errCh := o.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case res := <-waitCh:
		if res.Error != nil {
			return 0, errors.New(res.Error.Message)
		}
		return res.StatusCode, nil
	case err := <-errCh:
		return 0, err
	}
}

// cleanup stops and removes all of a deployment's containers and its volume.
// Containers are addressed by their deterministic names, so cleanup works even
// for partially created deployments.
func (o *Orchestrator) cleanup(ctx context.Context, id string) {
	for _, name := range []string{proxyName(id), workerName(id), artifactsName(id)} {
		o.removeContainer(ctx, name)
	}
	_ = o.client.VolumeRemove(ctx, volumeName(id), true)
}

func (o *Orchestrator) removeContainer(ctx context.Context, ref string) {
	stopTimeout := 10
	_ = o.client.ContainerStop(ctx, ref, container.StopOptions{Timeout: &stopTimeout})
	_ = o.client.ContainerRemove(ctx, ref, container.RemoveOptions{Force: true})
}

// Verify Orchestrator implements deployment.Orchestrator.
var _ deployment.Orchestrator = (*Orchestrator)(nil)
