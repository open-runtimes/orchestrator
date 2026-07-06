// Package docker implements the pool.Orchestrator interface using the Docker
// API — the dev-parity backend, exec pools only (HTTP activations need the
// gateway routing of the Kubernetes backend). Each warm slot mirrors the pod
// shape: a one-shot shim-install container, then a deployments-sidecar and a
// workload container idling on the pool-shim FIFO, sharing a workspace
// volume. The daemon plus the sidecars are the source of truth: containers
// carry identity labels, and claim state is read live from each sidecar's
// /activation endpoint. Only a fast-path activation index is kept in memory,
// rebuilt from sidecar state on Start — activation-spec details (callback,
// artifacts) do not survive a restart on this backend.
package docker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/pool/claim"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"orchestrator/pkg/pool"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	tickInterval     = 2 * time.Second
	coldStartTimeout = 120 * time.Second
	defaultTimeout   = 300 // seconds, matches the service default
)

// activation is the in-memory fast-path record of a claimed slot. The
// sidecar's ClaimState is the durable record; this index only spares Status
// and List a sidecar round-trip per call.
type activation struct {
	id        string
	poolID    string
	slot      string
	claimedAt time.Time
}

// slotView groups one slot's containers, listed by labels.
type slotView struct {
	sidecar  *container.Summary
	workload *container.Summary
}

// Orchestrator implements pool.Orchestrator using Docker.
type Orchestrator struct {
	client *client.Client
	cfg    Config
	pools  map[string]pool.Pool
	http   *http.Client
	poster claim.Poster

	mu       sync.Mutex
	acts     map[string]*activation // by activation ID
	creating map[string]string      // slot ID → pool ID, in-flight slot creations

	loopCancel context.CancelFunc
	loopDone   chan struct{}
}

// NewOrchestrator creates a Docker pool orchestrator. Sandbox tiers need a
// RuntimeClass, so any non-runc pool is rejected up front.
func NewOrchestrator(_ context.Context, cfg Config) (*Orchestrator, error) {
	for i := range cfg.Pools {
		if s := cfg.Pools[i].Sandbox; s != "" && s != deployment.SandboxRunc {
			return nil, apperrors.Validation("sandbox", fmt.Sprintf("pool %q: sandbox tiers require the Kubernetes backend", cfg.Pools[i].ID))
		}
	}
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	pools := make(map[string]pool.Pool, len(cfg.Pools))
	for _, p := range cfg.Pools {
		pools[p.ID] = p
	}
	return &Orchestrator{
		client:   dockerClient,
		cfg:      cfg,
		pools:    pools,
		http:     &http.Client{},
		poster:   claim.NewPoster(),
		acts:     make(map[string]*activation),
		creating: make(map[string]string),
	}, nil
}

// Start rebuilds the activation index from the sidecars' claim state, then
// begins the replenishment loop (single service on Docker — no leader
// election needed).
func (o *Orchestrator) Start(ctx context.Context) error {
	o.reconcile(ctx)

	loopCtx, cancel := context.WithCancel(context.Background())
	o.loopCancel = cancel
	o.loopDone = make(chan struct{})
	go o.loop(loopCtx)
	return nil
}

// reconcile rebuilds the in-memory activation index by asking each running
// sidecar for its claim state — the durable record on this backend.
func (o *Orchestrator) reconcile(ctx context.Context) {
	count := 0
	for _, p := range o.cfg.Pools {
		views, err := o.slotsFor(ctx, p.ID)
		if err != nil {
			slog.Warn("Failed to reconcile pool", "poolId", p.ID, "error", err)
			continue
		}
		for slotID, s := range views {
			if s.sidecar == nil || s.sidecar.State != container.StateRunning {
				continue
			}
			cs, err := o.claimState(ctx, s.sidecar)
			if err != nil || !cs.Claimed || cs.Failed || cs.ActivationID == "" {
				continue
			}
			o.acts[cs.ActivationID] = &activation{id: cs.ActivationID, poolID: p.ID, slot: slotID, claimedAt: time.Now()}
			count++
		}
	}
	if count > 0 {
		slog.Warn("Reconstructed activations from sidecar claim state; activation spec details (callback, artifacts) are not recoverable on the Docker backend", "count", count)
	}
	slog.Info("Reconciled pools", "pools", len(o.cfg.Pools), "activations", count)
}

// loop is the replenishment controller: every tick it tops each pool up to
// Size, removes poisoned slots, and garbage-collects exited activations past
// retention.
func (o *Orchestrator) loop(ctx context.Context) {
	defer close(o.loopDone)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.tick(ctx)
		}
	}
}

func (o *Orchestrator) tick(ctx context.Context) {
	summaries, err := o.listManaged(ctx, filters.Arg("label", labelPoolID))
	if err != nil {
		slog.Warn("Failed to list pool containers", "error", err)
		return
	}
	byPool := groupSlots(summaries)

	// Slots of pools no longer configured are torn down: pools are config, so
	// removal is a config change honored here.
	for poolID, views := range byPool {
		if _, ok := o.pools[poolID]; ok {
			continue
		}
		for slotID := range views {
			slog.Info("Removing slot of unconfigured pool", "poolId", poolID, "slot", slotID)
			o.removeSlot(ctx, poolID, slotID)
		}
	}

	for _, p := range o.cfg.Pools {
		o.reconcilePool(ctx, p, byPool[p.ID])
	}
	o.gcVolumes(ctx, byPool)
	o.gcRecords(byPool)
}

// reconcilePool classifies each slot and replenishes the pool to Size. A slot
// counts toward size while it is unclaimed — warm, still starting, or being
// created; claimed and poisoned slots do not, which is what triggers
// replacement off the request path.
func (o *Orchestrator) reconcilePool(ctx context.Context, p pool.Pool, views map[string]*slotView) {
	available := 0
	for slotID, s := range views {
		if o.isCreating(slotID) {
			available++
			continue
		}
		switch o.classify(ctx, p.ID, slotID, s) {
		case slotWarm, slotStarting:
			available++
		case slotPoisoned:
			slog.Info("Removing poisoned pool slot", "poolId", p.ID, "slot", slotID)
			o.removeSlot(ctx, p.ID, slotID)
		case slotClaimed:
			o.gcClaimed(ctx, p.ID, slotID)
		}
	}

	// Creations still in flight that have not materialized containers yet.
	o.mu.Lock()
	for slotID, poolID := range o.creating {
		if poolID == p.ID {
			if _, seen := views[slotID]; !seen {
				available++
			}
		}
	}
	o.mu.Unlock()

	for i := available; i < p.Size; i++ {
		go func() {
			if _, err := o.createSlot(ctx, p); err != nil {
				slog.Warn("Failed to replenish pool slot", "poolId", p.ID, "error", err)
			}
		}()
	}
}

// slot classifications for the replenishment loop.
type slotClass int

const (
	slotStarting slotClass = iota // counts toward size, not claimable yet
	slotWarm                      // healthy and unclaimed
	slotClaimed                   // bound to an activation
	slotPoisoned                  // remove and replace
)

func (o *Orchestrator) classify(ctx context.Context, poolID, slotID string, s *slotView) slotClass {
	if s.sidecar == nil || s.sidecar.State != container.StateRunning {
		// A slot without a live sidecar can never be claimed. If it carries
		// an activation, retention GC owns it; otherwise it is debris.
		if o.recordForSlot(poolID, slotID) != nil {
			return slotClaimed
		}
		return slotPoisoned
	}

	health := o.sidecarHealth(ctx, s.sidecar.ID)
	cs, err := o.claimState(ctx, s.sidecar)
	switch {
	case err != nil:
		if health == container.Unhealthy {
			return slotPoisoned
		}
		return slotStarting
	case cs.Failed:
		return slotPoisoned
	case cs.Claimed:
		return slotClaimed
	case health == container.Healthy:
		return slotWarm
	case health == container.Unhealthy:
		return slotPoisoned
	default:
		return slotStarting
	}
}

// gcClaimed removes a claimed slot once its workload has been exited for
// longer than the retention window, dropping the activation record with it.
func (o *Orchestrator) gcClaimed(ctx context.Context, poolID, slotID string) {
	info, err := o.client.ContainerInspect(ctx, workloadName(poolID, slotID))
	if err != nil || info.State == nil || info.State.Running {
		return
	}
	finished, err := time.Parse(time.RFC3339Nano, info.State.FinishedAt)
	if err != nil || time.Since(finished) < o.cfg.Retention {
		return
	}
	slog.Info("Retiring exited activation slot", "poolId", poolID, "slot", slotID)
	o.removeSlot(ctx, poolID, slotID)
	if rec := o.recordForSlot(poolID, slotID); rec != nil {
		o.dropRecord(rec.id)
	}
}

// gcVolumes removes workspace volumes whose slot has no containers — debris
// from interrupted slot creations or teardowns.
func (o *Orchestrator) gcVolumes(ctx context.Context, byPool map[string]map[string]*slotView) {
	vols, err := o.client.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelManagedBy+"="+managedByValue),
			filters.Arg("label", labelPoolID),
		),
	})
	if err != nil {
		return
	}
	for _, vol := range vols.Volumes {
		poolID, slotID := vol.Labels[labelPoolID], vol.Labels[labelSlot]
		if _, exists := byPool[poolID][slotID]; exists || o.isCreating(slotID) {
			continue
		}
		_ = o.client.VolumeRemove(ctx, vol.Name, true)
	}
}

// gcRecords drops activation records whose slot containers are gone (e.g.
// removed by the activation timeout) once past retention.
func (o *Orchestrator) gcRecords(byPool map[string]map[string]*slotView) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, rec := range o.acts {
		if _, exists := byPool[rec.poolID][rec.slot]; exists {
			continue
		}
		if time.Since(rec.claimedAt) > o.cfg.Retention {
			delete(o.acts, id)
		}
	}
}

// Pools reports the configured pools with live warm/claimed counts.
func (o *Orchestrator) Pools(ctx context.Context) ([]pool.Status, error) {
	statuses := make([]pool.Status, 0, len(o.cfg.Pools))
	for _, p := range o.cfg.Pools {
		views, err := o.slotsFor(ctx, p.ID)
		if err != nil {
			return nil, apperrors.Internal("docker.listSlots", err)
		}
		status := pool.Status{ID: p.ID, Image: p.Image, Size: p.Size}
		for _, s := range views {
			if s.sidecar == nil || s.sidecar.State != container.StateRunning {
				continue
			}
			cs, err := o.claimState(ctx, s.sidecar)
			switch {
			case err != nil || cs.Failed:
			case cs.Claimed:
				status.Claimed++
			case o.sidecarHealth(ctx, s.sidecar.ID) == container.Healthy:
				status.Warm++
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// Activate claims a warm slot and late-binds the activation onto it, then
// blocks until the workload exits (bounded by TimeoutSeconds) and returns
// ExitCode/Output inline. Docker supports exec pools only.
func (o *Orchestrator) Activate(ctx context.Context, poolID string, act *pool.Activation) (*pool.ActivationStatus, error) {
	p, ok := o.pools[poolID]
	if !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	if p.HTTP() {
		return nil, apperrors.Validation("port", "pool "+poolID+" serves HTTP; HTTP pools require the Kubernetes backend — the Docker backend supports exec (run-to-completion) pools only")
	}
	if act.ID == "" {
		act.ID = randomHex(6)
	}
	timeoutSeconds := act.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTimeout
	}

	o.mu.Lock()
	if _, exists := o.acts[act.ID]; exists {
		o.mu.Unlock()
		return nil, apperrors.Conflict("activation", act.ID, "activation "+act.ID+" already exists")
	}
	o.mu.Unlock()

	slotID, err := o.claimSlot(ctx, p, act, timeoutSeconds)
	if err != nil {
		var poison *claim.Poison
		if errors.As(err, &poison) {
			return &pool.ActivationStatus{
				ID: act.ID, PoolID: poolID, PodID: slotPrefix(poolID, poison.Unit),
				State: pool.StateFailed, Error: poison.Msg,
			}, nil
		}
		return nil, err
	}

	rec := &activation{id: act.ID, poolID: poolID, slot: slotID, claimedAt: time.Now()}
	o.mu.Lock()
	o.acts[act.ID] = rec
	o.mu.Unlock()

	return o.awaitExit(ctx, rec, time.Duration(timeoutSeconds)*time.Second)
}

// claimSlot wins an unclaimed warm slot via the shared claim flow — the slot
// is the serialization point, so the service stays stateless. The bearer
// token is re-read from the sidecar's label (dev backend: the token never
// leaves the local daemon).
func (o *Orchestrator) claimSlot(ctx context.Context, p pool.Pool, act *pool.Activation, timeoutSeconds int) (string, error) {
	req := &proxy.ClaimRequest{
		ActivationID:   act.ID,
		Command:        act.Command,
		Environment:    act.Environment,
		Artifacts:      act.Artifacts,
		Port:           p.Port,
		TimeoutSeconds: timeoutSeconds,
	}
	unit, err := claim.Claim(ctx, &slotInventory{o: o, p: p}, o.poster, p.ID, p.Burst, req)
	if err != nil {
		return "", err
	}
	return unit.ID, nil
}

// slotInventory is the Docker warm-unit surface behind the claim flow's
// seam: free units are healthy, unclaimed slots, a cold create pays the
// burst cold start.
type slotInventory struct {
	o *Orchestrator
	p pool.Pool
}

func (inv *slotInventory) Free(ctx context.Context) ([]claim.Unit, error) {
	views, err := inv.o.slotsFor(ctx, inv.p.ID)
	if err != nil {
		return nil, apperrors.Internal("docker.listSlots", err)
	}
	var units []claim.Unit
	for slotID, s := range views {
		if inv.o.isCreating(slotID) || s.sidecar == nil || s.sidecar.State != container.StateRunning {
			continue
		}
		if inv.o.sidecarHealth(ctx, s.sidecar.ID) != container.Healthy {
			continue
		}
		// Container labels cannot show live claims, so ask the sidecar — its
		// ClaimState is the durable record.
		cs, err := inv.o.claimState(ctx, s.sidecar)
		if err != nil || cs.Claimed || cs.Failed {
			continue
		}
		if unit, ok := inv.o.slotUnit(slotID, s.sidecar); ok {
			units = append(units, unit)
		}
	}
	return units, nil
}

// Create provisions a slot and waits for its sidecar to turn healthy
// (bounded); a slot that never warms is removed so the burst does not leak
// capacity beyond the pool size.
func (inv *slotInventory) Create(ctx context.Context) (*claim.Unit, error) {
	slotID, err := inv.o.createSlot(ctx, inv.p)
	if err != nil {
		return nil, err
	}
	if err := inv.o.waitSidecarHealthy(ctx, inv.p.ID, slotID); err != nil {
		inv.o.removeSlot(context.WithoutCancel(ctx), inv.p.ID, slotID)
		return nil, apperrors.Internal("docker.coldSlot", err)
	}
	summaries, err := inv.o.listManaged(ctx,
		filters.Arg("label", labelPoolID+"="+inv.p.ID),
		filters.Arg("label", labelSlot+"="+slotID),
		filters.Arg("label", labelType+"="+typeSidecar))
	if err != nil || len(summaries) == 0 {
		return nil, apperrors.Internal("docker.findSidecar", fmt.Errorf("sidecar for slot %s not found: %w", slotID, err))
	}
	unit, ok := inv.o.slotUnit(slotID, &summaries[0])
	if !ok {
		return nil, apperrors.Internal("docker.findSidecar", errors.New("sidecar has no IP address"))
	}
	return &unit, nil
}

// slotUnit maps a slot's sidecar container to a claimable unit.
func (o *Orchestrator) slotUnit(slotID string, sidecar *container.Summary) (claim.Unit, bool) {
	ip := o.summaryIP(sidecar)
	if ip == "" {
		return claim.Unit{}, false
	}
	return claim.Unit{ID: slotID, Addr: ip, Token: sidecar.Labels[labelClaimToken]}, true
}

// awaitExit blocks until the claimed workload exits and returns its exit code
// and captured output; on timeout the slot is torn down and the activation
// reported failed.
func (o *Orchestrator) awaitExit(ctx context.Context, rec *activation, timeout time.Duration) (*pool.ActivationStatus, error) {
	status := &pool.ActivationStatus{ID: rec.id, PoolID: rec.poolID, PodID: slotPrefix(rec.poolID, rec.slot)}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err := o.waitForExit(waitCtx, workloadName(rec.poolID, rec.slot))
	if err != nil {
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			o.removeSlot(context.WithoutCancel(ctx), rec.poolID, rec.slot)
			status.State = pool.StateFailed
			status.Error = fmt.Sprintf("activation timed out after %s", timeout)
			return status, nil
		}
		return nil, apperrors.Internal("docker.waitWorkload", err)
	}

	exitCode := int(code)
	status.State = pool.StateExited
	status.ExitCode = &exitCode
	status.Output = o.collectOutput(context.WithoutCancel(ctx), workloadName(rec.poolID, rec.slot))
	return status, nil
}

// Status returns one activation's state, derived from the workload container.
func (o *Orchestrator) Status(ctx context.Context, poolID, activationID string) (*pool.ActivationStatus, error) {
	o.mu.Lock()
	rec := o.acts[activationID]
	o.mu.Unlock()
	if rec == nil || rec.poolID != poolID {
		return nil, apperrors.NotFound("activation", activationID)
	}
	return o.statusFor(ctx, rec), nil
}

// List returns the pool's live activations.
func (o *Orchestrator) List(ctx context.Context, poolID string) ([]pool.ActivationStatus, error) {
	if _, ok := o.pools[poolID]; !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	o.mu.Lock()
	recs := make([]*activation, 0, len(o.acts))
	for _, rec := range o.acts {
		if rec.poolID == poolID {
			recs = append(recs, rec)
		}
	}
	o.mu.Unlock()
	slices.SortFunc(recs, func(a, b *activation) int { return cmp.Compare(a.id, b.id) })

	statuses := make([]pool.ActivationStatus, 0, len(recs))
	for _, rec := range recs {
		statuses = append(statuses, *o.statusFor(ctx, rec))
	}
	return statuses, nil
}

// statusFor derives an activation's status from its workload container:
// running → ready, exited → exited (+ code and output), gone → failed.
func (o *Orchestrator) statusFor(ctx context.Context, rec *activation) *pool.ActivationStatus {
	status := &pool.ActivationStatus{ID: rec.id, PoolID: rec.poolID, PodID: slotPrefix(rec.poolID, rec.slot)}

	name := workloadName(rec.poolID, rec.slot)
	info, err := o.client.ContainerInspect(ctx, name)
	exists := err == nil && info.State != nil
	running := exists && info.State.Running

	status.State = activationState(exists, running)
	switch status.State {
	case pool.StateFailed:
		status.Error = "workload container gone"
	case pool.StateExited:
		exitCode := info.State.ExitCode
		status.ExitCode = &exitCode
		status.Output = o.collectOutput(ctx, name)
	}
	return status
}

// Deactivate tears the activation's slot down; the loop replenishes off the
// request path.
func (o *Orchestrator) Deactivate(ctx context.Context, poolID, activationID string) error {
	o.mu.Lock()
	rec := o.acts[activationID]
	o.mu.Unlock()
	if rec == nil || rec.poolID != poolID {
		return apperrors.NotFound("activation", activationID)
	}
	o.removeSlot(ctx, poolID, rec.slot)
	o.dropRecord(activationID)
	return nil
}

// Ready checks if the Docker daemon is reachable and responsive.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Ping(ctx)
	return err
}

// Close stops the replenishment loop and releases the Docker client. Warm
// slots and running activations are left as-is — Start reconciles them.
func (o *Orchestrator) Close() error {
	if o.loopCancel != nil {
		o.loopCancel()
		<-o.loopDone
	}
	return o.client.Close()
}

// createSlot provisions one warm slot: workspace volume, one-shot
// shim-install, then the workload (idling on the shim FIFO) and its sidecar.
// The slot counts toward pool size the moment creation starts.
func (o *Orchestrator) createSlot(ctx context.Context, p pool.Pool) (string, error) {
	slotID := randomHex(4)
	o.mu.Lock()
	o.creating[slotID] = p.ID
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.creating, slotID)
		o.mu.Unlock()
	}()

	if err := o.provisionSlot(ctx, p, slotID); err != nil {
		o.removeSlot(context.WithoutCancel(ctx), p.ID, slotID)
		return "", err
	}
	return slotID, nil
}

func (o *Orchestrator) provisionSlot(ctx context.Context, p pool.Pool, slotID string) error {
	// Detached context so an HTTP request timeout doesn't cancel image pulls.
	pullCtx := context.WithoutCancel(ctx)
	for _, img := range []string{p.Image, o.cfg.SidecarImage, o.cfg.ShimImage} {
		if err := o.pullImageIfNeeded(pullCtx, img); err != nil {
			return apperrors.Internal("docker.pullImage", err)
		}
	}

	if _, err := o.client.VolumeCreate(ctx, volume.CreateOptions{
		Name:   volumeName(p.ID, slotID),
		Labels: slotLabels(p.ID, slotID, ""),
	}); err != nil {
		return apperrors.Internal("docker.createVolume", err)
	}

	if err := o.installShim(ctx, p.ID, slotID); err != nil {
		return err
	}
	if err := o.startWorkload(ctx, p, slotID); err != nil {
		return err
	}
	return o.startSidecar(ctx, p, slotID)
}

// installShim runs the one-shot shim-install container: the pool image is the
// user's runtime and has no shim, so the shim image copies its binary into
// the shared workspace. Root, like the deployment backend's artifacts step —
// the ko image is nonroot but the fresh volume is root-owned.
func (o *Orchestrator) installShim(ctx context.Context, poolID, slotID string) error {
	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:  o.cfg.ShimImage,
			Cmd:    []string{"-install", shimPath},
			User:   "0",
			Labels: slotLabels(poolID, slotID, typeInstall),
		},
		&container.HostConfig{Mounts: []mount.Mount{o.workspaceMount(poolID, slotID)}},
		nil, nil, installName(poolID, slotID))
	if err != nil {
		return apperrors.Internal("docker.createShimInstall", err)
	}
	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startShimInstall", err)
	}
	exitCode, err := o.waitForExit(ctx, resp.ID)
	if err != nil {
		return apperrors.Internal("docker.waitShimInstall", err)
	}
	o.removeContainer(ctx, resp.ID)
	if exitCode != 0 {
		return apperrors.Internal("docker.shimInstall",
			fmt.Errorf("shim-install exited with code %d", exitCode))
	}
	return nil
}

// startWorkload starts the pool-image container with its entrypoint
// overridden to the installed shim, which idles on the FIFO until claimed.
func (o *Orchestrator) startWorkload(ctx context.Context, p pool.Pool, slotID string) error {
	env := []string{"SHARED_VOLUME_PATH=" + workspacePath}
	for k, v := range p.Environment {
		env = append(env, k+"="+v)
	}

	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:      p.Image,
			Entrypoint: []string{shimPath},
			Env:        env,
			WorkingDir: workspacePath,
			Labels:     slotLabels(p.ID, slotID, typeWorkload),
		},
		&container.HostConfig{
			Mounts: []mount.Mount{o.workspaceMount(p.ID, slotID)},
			Resources: container.Resources{
				NanoCPUs: int64(p.CPU * 1e9),
				Memory:   int64(p.Memory) * 1024 * 1024,
			},
		},
		o.networkingConfig(), nil, workloadName(p.ID, slotID))
	if err != nil {
		return apperrors.Internal("docker.createWorkload", err)
	}
	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startWorkload", err)
	}
	return nil
}

// startSidecar starts the deployments-sidecar in pool mode: armed with the
// claim token, it starts unclaimed and reports ready (healthy) while
// warm-unclaimed. The token also goes on a label so the service can re-read
// it statelessly. Root because a claim writes into the root-owned workspace
// (artifacts) and opens the workload-created FIFO.
func (o *Orchestrator) startSidecar(ctx context.Context, p pool.Pool, slotID string) error {
	token := randomHex(16)
	labels := slotLabels(p.ID, slotID, typeSidecar)
	labels[labelClaimToken] = token

	healthCheck := &container.HealthConfig{
		Test:          []string{"CMD", "/ko-app/deployments-sidecar", "-check-ready"},
		Interval:      500 * time.Millisecond,
		Timeout:       5 * time.Second,
		StartPeriod:   coldStartTimeout,
		StartInterval: 500 * time.Millisecond,
	}

	resp, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image: o.cfg.SidecarImage,
			Env: []string{
				proxy.EnvClaimToken + "=" + token,
				"SHARED_VOLUME_PATH=" + workspacePath,
				proxy.EnvTargetHost + "=" + workloadName(p.ID, slotID),
			},
			User:        "0",
			Healthcheck: healthCheck,
			Labels:      labels,
		},
		&container.HostConfig{
			Mounts:     []mount.Mount{o.workspaceMount(p.ID, slotID)},
			ExtraHosts: o.cfg.ExtraHosts,
		},
		o.networkingConfig(), nil, sidecarName(p.ID, slotID))
	if err != nil {
		return apperrors.Internal("docker.createSidecar", err)
	}
	if err := o.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return apperrors.Internal("docker.startSidecar", err)
	}
	return nil
}

// waitSidecarHealthy polls until the slot's sidecar reports healthy — the
// cold-create claimability gate.
func (o *Orchestrator) waitSidecarHealthy(ctx context.Context, poolID, slotID string) error {
	deadline := time.Now().Add(coldStartTimeout)
	for {
		if o.sidecarHealth(ctx, sidecarName(poolID, slotID)) == container.Healthy {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sidecar not healthy within %s", coldStartTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// sidecarHealth returns the sidecar's Docker health status ("" if unknown).
func (o *Orchestrator) sidecarHealth(ctx context.Context, ref string) container.HealthStatus {
	info, err := o.client.ContainerInspect(ctx, ref)
	if err != nil || info.State == nil || info.State.Health == nil {
		return ""
	}
	return info.State.Health.Status
}

// claimState asks the sidecar for its authoritative claim record.
func (o *Orchestrator) claimState(ctx context.Context, sidecar *container.Summary) (*proxy.ClaimState, error) {
	ip := o.summaryIP(sidecar)
	if ip == "" {
		return nil, errors.New("sidecar has no IP address")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, o.sidecarURL(ip, proxy.ClaimStatePath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claim state returned status %d", resp.StatusCode)
	}
	state := &proxy.ClaimState{}
	if err := json.NewDecoder(resp.Body).Decode(state); err != nil {
		return nil, err
	}
	return state, nil
}

func (o *Orchestrator) sidecarURL(ip, path string) string {
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(proxy.DefaultAdminPort)) + path
}

func (o *Orchestrator) summaryIP(c *container.Summary) string {
	if c.NetworkSettings == nil {
		return ""
	}
	return containerIP(c.NetworkSettings.Networks, o.cfg.Network)
}

// collectOutput reads the workload's combined stdout+stderr, capped at
// maxOutputBytes.
func (o *Orchestrator) collectOutput(ctx context.Context, name string) string {
	logs, err := o.client.ContainerLogs(ctx, name, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer logs.Close()
	out := &cappedWriter{cap: maxOutputBytes}
	_, _ = stdcopy.StdCopy(out, out, logs)
	return out.String()
}

// slotsFor lists a pool's containers and groups them into slot views.
func (o *Orchestrator) slotsFor(ctx context.Context, poolID string) (map[string]*slotView, error) {
	summaries, err := o.listManaged(ctx, filters.Arg("label", labelPoolID+"="+poolID))
	if err != nil {
		return nil, err
	}
	return groupSlots(summaries)[poolID], nil
}

// groupSlots indexes container summaries by pool and slot label.
func groupSlots(summaries []container.Summary) map[string]map[string]*slotView {
	byPool := make(map[string]map[string]*slotView)
	for i := range summaries {
		c := &summaries[i]
		poolID, slotID := c.Labels[labelPoolID], c.Labels[labelSlot]
		if poolID == "" || slotID == "" {
			continue
		}
		if byPool[poolID] == nil {
			byPool[poolID] = make(map[string]*slotView)
		}
		s := byPool[poolID][slotID]
		if s == nil {
			s = &slotView{}
			byPool[poolID][slotID] = s
		}
		switch c.Labels[labelType] {
		case typeSidecar:
			s.sidecar = c
		case typeWorkload:
			s.workload = c
		}
	}
	return byPool
}

// listManaged lists all containers carrying the deployments-service label,
// narrowed by extra filters.
func (o *Orchestrator) listManaged(ctx context.Context, extra ...filters.KeyValuePair) ([]container.Summary, error) {
	pairs := append([]filters.KeyValuePair{
		filters.Arg("label", labelManagedBy+"="+managedByValue),
	}, extra...)
	return o.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(pairs...),
	})
}

func (o *Orchestrator) isCreating(slotID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.creating[slotID]
	return ok
}

// recordForSlot returns the activation record bound to a slot, if any.
func (o *Orchestrator) recordForSlot(poolID, slotID string) *activation {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, rec := range o.acts {
		if rec.poolID == poolID && rec.slot == slotID {
			return rec
		}
	}
	return nil
}

func (o *Orchestrator) dropRecord(activationID string) {
	o.mu.Lock()
	delete(o.acts, activationID)
	o.mu.Unlock()
}

// removeSlot tears down a slot's containers and workspace volume. Containers
// are addressed by their deterministic names, so this works for partially
// created slots; already-gone containers are ignored.
func (o *Orchestrator) removeSlot(ctx context.Context, poolID, slotID string) {
	for _, name := range []string{sidecarName(poolID, slotID), workloadName(poolID, slotID), installName(poolID, slotID)} {
		o.removeContainer(ctx, name)
	}
	_ = o.client.VolumeRemove(ctx, volumeName(poolID, slotID), true)
}

func (o *Orchestrator) removeContainer(ctx context.Context, ref string) {
	stopTimeout := 10
	_ = o.client.ContainerStop(ctx, ref, container.StopOptions{Timeout: &stopTimeout})
	_ = o.client.ContainerRemove(ctx, ref, container.RemoveOptions{Force: true})
}

func (o *Orchestrator) workspaceMount(poolID, slotID string) mount.Mount {
	return mount.Mount{Type: mount.TypeVolume, Source: volumeName(poolID, slotID), Target: workspacePath}
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

// Verify Orchestrator implements pool.Orchestrator.
var _ pool.Orchestrator = (*Orchestrator)(nil)
