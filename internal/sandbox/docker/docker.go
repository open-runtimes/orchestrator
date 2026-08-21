// Package docker implements the sandbox.Orchestrator interface using the Docker
// API — the development backend. Each sandbox is a worker container running the
// pool's image, fronted by a workload-sidecar proxy container, with an
// optional one-shot artifacts step, all sharing a workspace volume. The daemon
// is the source of truth: the volume is the identity anchor and carries the
// spec and the capability token on its labels, so listing volumes reconstructs
// every sandbox and the service holds nothing.
//
// Two things it deliberately does NOT do, both documented in
// docs/sandboxes.md: there is no warm pool (a create pays a cold container
// start, where Kubernetes claims a running pod in well under a second), and
// there are no isolation tiers (gvisor and kata are RuntimeClasses, which
// Docker has no equivalent of). Sandboxes here are for developing against the
// API, not for running untrusted code.
package docker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/moby"
	"orchestrator/internal/pool"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/workload"
	"slices"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Orchestrator implements sandbox.Orchestrator using Docker.
type Orchestrator struct {
	client *client.Client
	cfg    Config
	addr   sandbox.Addressing
	pools  map[string]*pool.Pool
	stop   context.CancelFunc

	// now and tick are shrunk (and frozen) by tests.
	now  func() time.Time
	tick time.Duration
}

// NewOrchestrator creates a Docker sandbox orchestrator.
func NewOrchestrator(_ context.Context, cfg Config) (*Orchestrator, error) {
	cfg.applyDefaults()
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return &Orchestrator{
		client: dockerClient,
		cfg:    cfg,
		addr:   cfg.addressing(),
		pools:  pool.ByID(cfg.Pools),
		now:    time.Now,
		tick:   reapTick,
	}, nil
}

// Start reports what exists and launches the idle sweep. Sandboxes are
// self-sufficient containers, so there is nothing to resume.
func (o *Orchestrator) Start(ctx context.Context) error {
	statuses, err := o.List(ctx)
	if err != nil {
		slog.Warn("Failed to reconcile sandboxes", "error", err)
	} else {
		slog.Info("Reconciled sandboxes", "count", len(statuses))
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.stop = cancel
	go o.runReaper(runCtx)
	return nil
}

// Pools reports the configured sandbox pools. Warm is always zero: Docker
// pre-warms nothing, so every create is a cold start.
func (o *Orchestrator) Pools(ctx context.Context) ([]pool.Status, error) {
	claimed := make(map[string]int, len(o.cfg.Pools))
	volumes, err := o.volumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, vol := range volumes {
		claimed[vol.Labels[labelPool]]++
	}
	statuses := make([]pool.Status, 0, len(o.cfg.Pools))
	for i := range o.cfg.Pools {
		p := &o.cfg.Pools[i]
		statuses = append(statuses, pool.Status{ID: p.ID, Image: p.Image, Size: p.Size, Claimed: claimed[p.ID]})
	}
	return statuses, nil
}

// Create provisions the sandbox: the labeled workspace volume that IS the
// sandbox, its artifacts, the worker, and the proxy — then waits for the
// proxy's health check to report the contract serving.
func (o *Orchestrator) Create(ctx context.Context, req *sandbox.Request) (*sandbox.Status, error) {
	// A declared pool's shape, or the one the request describes. Docker creates
	// the container either way — it has no warm capacity — so poolless costs it
	// nothing extra.
	shape := req.Shape()
	if req.Pool != "" {
		p := o.pools[req.Pool]
		if p == nil {
			return nil, apperrors.NotFound("pool", req.Pool)
		}
		shape = p.Spec
	}
	// Mounts are a Kubernetes capability: a pod's containers can share a mount
	// through propagation on a shared volume, which sibling Docker containers
	// cannot. Say so rather than accepting the request and failing the claim.
	if artifact.HasMount(req.Artifacts) {
		return nil, apperrors.Validation("artifacts",
			"the Docker backend cannot mount: sibling containers do not share a mount namespace (see docs/sandboxes.md#the-docker-backend)")
	}
	if _, err := o.client.VolumeInspect(ctx, volumeName(req.ID)); err == nil {
		return nil, apperrors.Conflict("sandbox", req.ID, "sandbox "+req.ID+" already exists")
	} else if !cerrdefs.IsNotFound(err) {
		return nil, apperrors.Internal("docker.inspectVolume", err)
	}

	spec, err := json.Marshal(req.Recorded(shape))
	if err != nil {
		return nil, apperrors.Internal("docker.marshalSpec", err)
	}
	if _, err := o.client.VolumeCreate(ctx, volume.CreateOptions{
		Name:   volumeName(req.ID),
		Labels: volumeLabels(req, string(spec)),
	}); err != nil {
		return nil, apperrors.Internal("docker.createVolume", err)
	}

	status, err := o.materialize(ctx, &shape, req)
	if err != nil {
		o.cleanup(ctx, req.ID)
		return nil, err
	}
	if status.State == sandbox.StateFailed {
		// Artifacts failed, or the image never served: nothing is running and
		// no URL was handed out, so the sandbox is gone rather than broken.
		o.cleanup(ctx, req.ID)
	}
	return status, nil
}

// materialize runs artifacts, starts the containers, and waits for serving.
func (o *Orchestrator) materialize(ctx context.Context, p *pool.Spec, req *sandbox.Request) (*sandbox.Status, error) {
	// Detached context so an HTTP request timeout doesn't cancel image pulls.
	pullCtx := context.WithoutCancel(ctx)
	if len(req.Artifacts) > 0 {
		if err := moby.PullImage(pullCtx, o.client, o.cfg.JobSidecarImage); err != nil {
			return nil, apperrors.Internal("docker.pullJobSidecarImage", err)
		}
		if err := o.runArtifacts(ctx, req); err != nil {
			// A failed artifact is the sandbox's failure, not the API's — same
			// as a poisoned pod on Kubernetes, so it is reported as a failed
			// sandbox rather than returned as an error.
			//nolint:nilerr // see above: the failure is the status, not the call
			return &sandbox.Status{ID: req.ID, PoolID: req.Pool, State: sandbox.StateFailed, Error: err.Error()}, nil
		}
	}
	if err := moby.PullImage(pullCtx, o.client, p.Image); err != nil {
		return nil, apperrors.Internal("docker.pullImage", err)
	}
	if err := moby.PullImage(pullCtx, o.client, o.cfg.SidecarImage); err != nil {
		return nil, apperrors.Internal("docker.pullSidecarImage", err)
	}
	if err := o.installAgent(ctx, pullCtx, req.ID); err != nil {
		return nil, err
	}

	workerIP, err := o.startWorker(ctx, p, req)
	if err != nil {
		return nil, err
	}
	if err := o.startProxy(ctx, p, req, workerIP); err != nil {
		return nil, err
	}
	return o.awaitServing(ctx, p, req)
}

// awaitServing waits for the proxy's health check, so a 201 means the URL is
// live. The worker exiting first is a failure — a sandbox has no business
// exiting.
func (o *Orchestrator) awaitServing(ctx context.Context, p *pool.Spec, req *sandbox.Request) (*sandbox.Status, error) {
	status := &sandbox.Status{
		ID:     req.ID,
		PoolID: req.Pool,
		URL:    o.addr.URL(req.Token),
		URLs:   o.addr.URLs(req.Token, p.Port, req.Ports),
		Image:  p.Image,
		CPU:    p.CPU,
		Memory: p.Memory,
	}
	deadline := o.now().Add(servingWait)
	for {
		state, err := o.serving(ctx, req.ID)
		switch {
		case err != nil:
			return nil, err
		case state == sandbox.StateReady:
			status.State = sandbox.StateReady
			return status, nil
		case state == sandbox.StateFailed:
			return &sandbox.Status{
				ID: req.ID, PoolID: req.Pool, State: sandbox.StateFailed,
				Error: "sandbox exited before it served",
			}, nil
		}
		if o.now().After(deadline) {
			return &sandbox.Status{
				ID: req.ID, PoolID: req.Pool, State: sandbox.StateFailed,
				Error: fmt.Sprintf("sandbox not serving within %s", servingWait),
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

const (
	servingWait    = 120 * time.Second // cold start: image pull plus artifacts
	pollInterval   = 250 * time.Millisecond
	probeTimeout   = 500 * time.Millisecond // bounds one reachability probe
	cleanupTimeout = 30 * time.Second       // bounds removing a sandbox whose caller may be gone
)

// serving derives a sandbox's live state from its containers.
func (o *Orchestrator) serving(ctx context.Context, id string) (string, error) {
	worker, err := o.inspect(ctx, workerName(id))
	if err != nil {
		return "", err
	}
	if worker == nil || worker.State == nil {
		return sandbox.StateCreating, nil
	}
	if !worker.State.Running {
		return sandbox.StateFailed, nil
	}
	prox, err := o.inspect(ctx, proxyName(id))
	if err != nil {
		return "", err
	}
	if prox == nil || prox.State == nil || !prox.State.Running {
		return sandbox.StateCreating, nil
	}
	if prox.State.Health != nil && prox.State.Health.Status == container.Healthy {
		return sandbox.StateReady, nil
	}
	return sandbox.StateCreating, nil
}

// runArtifacts materializes req.Artifacts into the workspace volume via a
// one-shot job-sidecar pre-mode run — the same path jobs and deployments use.
func (o *Orchestrator) runArtifacts(ctx context.Context, req *sandbox.Request) error {
	artifactsJSON, err := artifact.MarshalArtifacts(req.Artifacts)
	if err != nil {
		return apperrors.Internal("docker.marshalArtifacts", err)
	}
	env := []string{
		"JOB_ID=sbx-" + req.ID,
		config.EnvSharedVolume + "=" + workspacePath,
		"ARTIFACTS_JSON=" + string(artifactsJSON),
	}
	if o.cfg.ArtifactEndpoint != "" {
		env = append(env, "ARTIFACT_ENDPOINT="+o.cfg.ArtifactEndpoint)
	}
	for _, kv := range config.LoadS3Credentials().ToEnv() {
		env = append(env, kv[0]+"="+kv[1])
	}

	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:  o.cfg.JobSidecarImage,
			Cmd:    []string{"-mode=pre"},
			Env:    env,
			User:   "0",
			Labels: containerLabels(req.ID, typeArtifacts),
		},
		&container.HostConfig{
			Mounts:     []mount.Mount{o.workspaceMount(req.ID)},
			ExtraHosts: o.cfg.ExtraHosts,
		},
		moby.NetworkingConfig(o.cfg.Network), nil, artifactsName(req.ID))
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
		return fmt.Errorf("artifacts failed: container exited with code %d", exitCode)
	}
	return nil
}

// installAgent copies the sandbox agent out of the image that publishes it into
// the sandbox's workspace volume — the same move as the agent-install init
// container on Kubernetes, and what lets a pool run an ordinary runtime image
// that serves the contract without implementing it.
func (o *Orchestrator) installAgent(ctx, pullCtx context.Context, id string) error {
	if err := moby.PullImage(pullCtx, o.client, o.cfg.AgentImage); err != nil {
		return apperrors.Internal("docker.pullAgentImage", err)
	}
	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:      o.cfg.AgentImage,
			Entrypoint: []string{"cp"},
			Cmd:        []string{sandbox.AgentSource, agentPath},
			// Root, like the artifacts step: a Docker volume takes its ownership
			// from whichever image mounts it first, so the copy cannot assume the
			// agent image's own user can write there. The binary lands 0755, so
			// the worker reads it whatever user it runs as. (On Kubernetes the
			// workspace is a world-writable emptyDir and the install container
			// keeps the hardening floor.)
			User:   "0",
			Labels: containerLabels(id, typeAgent),
		},
		&container.HostConfig{Mounts: []mount.Mount{o.workspaceMount(id)}},
		nil, nil, agentName(id))
	if err != nil {
		return apperrors.Internal("docker.createAgentInstaller", err)
	}
	defer o.removeContainer(ctx, resp.ID)

	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startAgentInstaller", err)
	}
	code, err := o.waitForExit(ctx, resp.ID)
	if err != nil {
		return apperrors.Internal("docker.waitAgentInstaller", err)
	}
	if code != 0 {
		return apperrors.Internal("docker.agentInstaller",
			fmt.Errorf("agent install exited with code %d", code))
	}
	return nil
}

// startWorker runs the pool's image with the sandbox's command, returning the
// container's address on the configured network.
func (o *Orchestrator) startWorker(ctx context.Context, p *pool.Spec, req *sandbox.Request) (string, error) {
	// The agent's own settings first, so a pool or request can override them.
	env := []string{
		"SANDBOX_PORT=" + strconv.Itoa(p.Port),
		"SANDBOX_WORKSPACE=" + workspacePath,
	}
	for k, v := range p.Environment {
		env = append(env, k+"="+v)
	}
	for k, v := range req.Environment {
		env = append(env, k+"="+v)
	}

	// The command falls back from the request to the pool to the installed agent,
	// so a plain runtime image serves the contract with nothing named anywhere.
	// Exec'd directly rather than through a shell: an arbitrary image may not
	// have one (distroless), and the agent needs no shell features.
	cmd := []string{agentPath}
	if command := cmp.Or(req.Command, p.Command); command != "" {
		cmd = []string{"/bin/sh", "-c", command}
	}
	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:      p.Image,
			Cmd:        cmd,
			Env:        env,
			WorkingDir: workspacePath,
			Labels:     containerLabels(req.ID, typeWorker),
		},
		&container.HostConfig{
			Mounts: append([]mount.Mount{o.workspaceMount(req.ID)}, moby.Mounts(p.Volumes)...),
			Resources: container.Resources{
				NanoCPUs: int64(p.CPU * 1e9),
				Memory:   int64(p.Memory) * 1024 * 1024,
			},
			ExtraHosts: o.cfg.ExtraHosts,
		},
		moby.NetworkingConfig(o.cfg.Network), nil, workerName(req.ID))
	if err != nil {
		return "", apperrors.Internal("docker.createWorker", err)
	}
	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", apperrors.Internal("docker.startWorker", err)
	}
	info, err := o.client.ContainerInspect(ctx, resp.ID)
	if err != nil {
		return "", apperrors.Internal("docker.inspectWorker", err)
	}
	ip := containerIP(info.NetworkSettings, o.cfg.Network)
	if ip == "" {
		return "", apperrors.Internal("docker.workerIP", errors.New("worker container has no IP address"))
	}
	return ip, nil
}

// startProxy fronts the worker with the workload-sidecar in direct mode: it
// owns readiness, the per-request timeout, the request counter the idle sweep
// reads, and the port hint for the sandbox's extra ports.
func (o *Orchestrator) startProxy(ctx context.Context, p *pool.Spec, req *sandbox.Request, workerIP string) error {
	labels := containerLabels(req.ID, typeProxy)
	labels[labelToken] = req.Token

	env := []string{
		workload.EnvTarget + "=" + net.JoinHostPort(workerIP, strconv.Itoa(p.Port)),
		workload.EnvTargetHost + "=" + workerIP,
	}
	if len(req.Ports) > 0 {
		env = append(env, workload.EnvExtraPorts+"="+portList(req.Ports))
	}
	// An explicit 0 means no per-request bound (long-lived sessions); nil takes
	// the sidecar's own default, exactly as on the claim path.
	if req.TimeoutSeconds != nil {
		env = append(env, workload.EnvTimeoutSeconds+"="+strconv.Itoa(*req.TimeoutSeconds))
	}

	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image: o.cfg.SidecarImage,
			Env:   env,
			Healthcheck: &container.HealthConfig{
				Test:          []string{"CMD", "/ko-app/workload-sidecar", "-check-ready"},
				Interval:      500 * time.Millisecond,
				Timeout:       5 * time.Second,
				StartPeriod:   servingWait,
				StartInterval: 500 * time.Millisecond,
			},
			Labels: labels,
		},
		&container.HostConfig{ExtraHosts: o.cfg.ExtraHosts},
		moby.NetworkingConfig(o.cfg.Network), nil, proxyName(req.ID))
	if err != nil {
		return apperrors.Internal("docker.createProxy", err)
	}
	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startProxy", err)
	}
	return nil
}

// Status reconstructs one sandbox from its volume and containers.
func (o *Orchestrator) Status(ctx context.Context, id string) (*sandbox.Status, error) {
	vol, err := o.client.VolumeInspect(ctx, volumeName(id))
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, apperrors.NotFound("sandbox", id)
		}
		return nil, apperrors.Internal("docker.inspectVolume", err)
	}
	status, err := o.statusFrom(ctx, vol.Labels)
	if err != nil {
		return nil, err
	}
	return &status, nil
}

// List returns every sandbox the daemon knows — one per managed volume.
func (o *Orchestrator) List(ctx context.Context) ([]sandbox.Status, error) {
	volumes, err := o.volumes(ctx)
	if err != nil {
		return nil, err
	}
	statuses := make([]sandbox.Status, 0, len(volumes))
	for _, vol := range volumes {
		status, err := o.statusFrom(ctx, vol.Labels)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// statusFrom derives a sandbox's API view from its volume labels plus the live
// container state.
func (o *Orchestrator) statusFrom(ctx context.Context, labels map[string]string) (sandbox.Status, error) {
	id := labels[labelID]
	status := sandbox.Status{ID: id, PoolID: labels[labelPool]}
	token := labels[labelToken]
	spec, err := parseSpec(labels[labelSpec])
	if err != nil {
		return status, err
	}
	// The primary port comes off the pool it was claimed from, or — for a
	// poolless sandbox, whose pool existed only in its own request — off the
	// stored spec. Reading it back must return the same addresses the create did.
	primary := spec.Port
	if p := o.pools[status.PoolID]; p != nil {
		primary = p.Port
	} else {
		status.PoolID = ""
	}
	if primary > 0 {
		status.URL = o.addr.URL(token)
		status.URLs = o.addr.URLs(token, primary, spec.Ports)
	}
	// Recorded at create, so this describes the pod that is running rather than
	// whatever its pool says today — the pool may have been re-imaged or removed
	// since. Absent for a sandbox created before the shape was recorded.
	status.Image, status.CPU, status.Memory = spec.Image, spec.CPU, spec.Memory
	state, err := o.serving(ctx, id)
	if err != nil {
		return status, err
	}
	status.State = state
	if state == sandbox.StateFailed {
		status.Error = "sandbox exited"
	}
	return status, nil
}

// Target resolves a capability token to the sandbox's proxy address — the
// activator.SandboxTargets seam. Nothing serves an unknown or torn-down token.
//
// The address is probed before it is handed back, because a container's IP is
// not necessarily routable from the host the instant the container starts: an
// unreachable target returns nil so the broker keeps holding the request, which
// is what the caller wants instead of an immediate 502. The Kubernetes edge
// probes for the same reason, one layer down.
func (o *Orchestrator) Target(ctx context.Context, token string) (*url.URL, error) {
	list, err := o.client.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelManagedBy+"="+managedByValue),
			filters.Arg("label", labelToken+"="+token),
		),
	})
	if err != nil {
		return nil, apperrors.Internal("docker.listContainers", err)
	}
	for i := range list {
		info, err := o.inspect(ctx, list[i].ID)
		if err != nil || info == nil || info.State == nil || !info.State.Running {
			continue
		}
		ip := containerIP(info.NetworkSettings, o.cfg.Network)
		if ip == "" || !o.reachable(ctx, ip) {
			continue
		}
		return &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(workload.DefaultProxyPort))}, nil
	}
	//nolint:nilnil // the activator.SandboxTargets contract: nil means nothing
	// serves this token, which the broker turns into a hold rather than an error
	return nil, nil
}

// reachable reports whether the sidecar's admin /ready answers from here.
func (o *Orchestrator) reachable(ctx context.Context, ip string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	probeURL := "http://" + net.JoinHostPort(ip, strconv.Itoa(workload.DefaultAdminPort)) + "/ready"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// Delete tears the sandbox down: its containers and the volume that carries its
// token, so a leaked URL dies with it.
func (o *Orchestrator) Delete(ctx context.Context, id string) error {
	if _, err := o.client.VolumeInspect(ctx, volumeName(id)); err != nil {
		if cerrdefs.IsNotFound(err) {
			return apperrors.NotFound("sandbox", id)
		}
		return apperrors.Internal("docker.inspectVolume", err)
	}
	o.cleanup(ctx, id)
	return nil
}

// Ready checks that the daemon is reachable.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Ping(ctx)
	return err
}

// Close stops the idle sweep and the daemon client. Running sandboxes are left
// alone — the daemon keeps them, and a restart reconciles from volumes.
func (o *Orchestrator) Close() error {
	if o.stop != nil {
		o.stop()
	}
	return o.client.Close()
}

// Verify Orchestrator implements sandbox.Orchestrator.
var _ sandbox.Orchestrator = (*Orchestrator)(nil)

// volumes lists the managed sandbox volumes — the sandbox set.
func (o *Orchestrator) volumes(ctx context.Context) ([]*volume.Volume, error) {
	list, err := o.client.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelManagedBy+"="+managedByValue), filters.Arg("label", labelID)),
	})
	if err != nil {
		return nil, apperrors.Internal("docker.listVolumes", err)
	}
	return list.Volumes, nil
}

// inspect returns a container, or (nil, nil) when it does not exist.
func (o *Orchestrator) inspect(ctx context.Context, name string) (*container.InspectResponse, error) {
	info, err := o.client.ContainerInspect(ctx, name)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			//nolint:nilnil // absence is not an error here: callers ask "is this
			// container there?" and a missing one is a legitimate answer
			return nil, nil
		}
		return nil, apperrors.Internal("docker.inspectContainer", err)
	}
	return &info, nil
}

// cleanup removes a sandbox's containers and its workspace volume. It detaches
// from the caller's context first: cleanup runs when a create failed or a delete
// was asked for, and a client that hung up mid-request has cancelled the very
// context the removals would ride on — which would leave the containers running.
func (o *Orchestrator) cleanup(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	for _, name := range []string{proxyName(id), workerName(id), artifactsName(id), agentName(id)} {
		o.removeContainer(ctx, name)
	}
	if err := o.client.VolumeRemove(ctx, volumeName(id), true); err != nil && !cerrdefs.IsNotFound(err) {
		slog.Warn("Failed to remove sandbox volume", "sandboxId", id, "error", err)
	}
}

func (o *Orchestrator) removeContainer(ctx context.Context, idOrName string) {
	err := o.client.ContainerRemove(ctx, idOrName, container.RemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		slog.Warn("Failed to remove container", "container", idOrName, "error", err)
	}
}

func (o *Orchestrator) waitForExit(ctx context.Context, containerID string) (int64, error) {
	statusCh, errCh := o.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return 0, err
	case status := <-statusCh:
		return status.StatusCode, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (o *Orchestrator) workspaceMount(id string) mount.Mount {
	return mount.Mount{Type: mount.TypeVolume, Source: volumeName(id), Target: workspacePath}
}

// containerIP returns the container's address on the configured network, or —
// when none is configured — the address of its first network by name, so the
// choice is deterministic with several attached. Read from Networks only: the
// top-level NetworkSettings.IPAddress is deprecated and modern daemons leave it
// empty.
func containerIP(settings *container.NetworkSettings, wanted string) string {
	if settings == nil {
		return ""
	}
	if wanted != "" {
		if endpoint, ok := settings.Networks[wanted]; ok {
			return endpoint.IPAddress
		}
		return ""
	}
	for _, name := range slices.Sorted(maps.Keys(settings.Networks)) {
		if ip := settings.Networks[name].IPAddress; ip != "" {
			return ip
		}
	}
	return ""
}
