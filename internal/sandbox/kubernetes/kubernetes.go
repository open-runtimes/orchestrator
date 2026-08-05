// Package kubernetes implements the sandbox.Orchestrator interface using the
// Kubernetes API. Everything about standing warm capacity — the pods, the
// claim, replenishment, poison and orphan GC, the idle rule — belongs to
// internal/warm. What a SANDBOX adds on top of a claimed pod is small on
// purpose: a capability token stamped as a label, and a wait for the image's
// contract to answer. There is no per-sandbox Service or route; the sandbox
// edge resolves the token from the request's Host behind one wildcard route,
// so a create is as fast as the claim and a delete churns nothing.
// See docs/sandboxes.md.
package kubernetes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/kube"
	"orchestrator/internal/proxy"
	"orchestrator/internal/warm"
	"orchestrator/pkg/pool"
	"orchestrator/pkg/sandbox"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultPoll = 500 * time.Millisecond
	// servingWait bounds the wait for a claimed sandbox to answer its contract
	// — artifact materialization plus image startup.
	servingWait = 60 * time.Second
)

// Label and annotation contract for sandbox pods. Deliberately distinct from
// the pool backend's keys: two consumers share the cluster, and each one's
// control loop must see exactly its own pods.
const (
	LabelPoolID    = "sandbox.pool"
	LabelSandboxID = "sandbox.id"
	// LabelToken carries the capability token the edge routes by. It is the
	// credential, so it lives only here — never in an annotation, a log line,
	// or an event payload — and dies with the pod on teardown.
	LabelToken = "sandbox.token"

	AnnotationSpec = "sandbox.spec"

	ManagedByValue = "deployments-service"
)

func naming() warm.Naming {
	return warm.Naming{
		ManagedBy:  ManagedByValue,
		Kind:       sandbox.MetricKind,
		Pool:       LabelPoolID,
		Claim:      LabelSandboxID,
		Spec:       AnnotationSpec,
		NamePrefix: "sbx",
		SecretName: "sandbox-claim-key",
	}
}

// Orchestrator implements sandbox.Orchestrator using Kubernetes.
type Orchestrator struct {
	client kubernetes.Interface
	warm   *warm.Manager
	cfg    Config
	stop   context.CancelFunc

	// Polling knobs, shrunk by unit tests.
	poll      time.Duration
	serveWait time.Duration
}

// NewOrchestrator creates a Kubernetes sandbox orchestrator.
func NewOrchestrator(_ context.Context, cfg Config) (*Orchestrator, error) {
	cfg.applyDefaults()
	cs, err := kube.NewClient(cfg.Kubeconfig, cfg.Context, cfg.Metrics)
	if err != nil {
		return nil, err
	}
	return wireOrchestrator(cs, cfg, nil), nil
}

// wireOrchestrator wires an Orchestrator around an injected client (tests pass
// a fake, and tune the warm layer: a fake sidecar client, since fake-clientset
// pods have no reachable IPs, and shorter waits). cfg must already have
// defaults applied.
func wireOrchestrator(cs kubernetes.Interface, cfg Config, tune func(*warm.Config)) *Orchestrator {
	warmCfg := cfg.warmConfig()
	if tune != nil {
		tune(&warmCfg)
	}
	return &Orchestrator{
		client:    cs,
		warm:      warm.New(cs, cfg.Pools, warmCfg),
		cfg:       cfg,
		poll:      defaultPoll,
		serveWait: servingWait,
	}
}

// Start surveys the existing warm/claimed pods, then launches the
// leader-elected control loop: replenishment, poison/orphan GC, and idle
// teardown.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.warm.Start(ctx); err != nil {
		return err
	}
	statuses, err := o.Pools(ctx)
	if err != nil {
		return err
	}
	for _, s := range statuses {
		slog.Info("Sandbox pool reconciled", "pool", s.ID, "size", s.Size, "warm", s.Warm, "claimed", s.Claimed)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.stop = cancel
	hooks := o.idleRule().Hooks()
	go kube.RunLeaderElected(runCtx, o.client, o.cfg.Namespace, o.cfg.LeaderElection,
		func(loopCtx context.Context) { o.warm.RunControl(loopCtx, hooks) }, o.onLeadership)
	return nil
}

// idleRule tears a sandbox down after its idle window passes with no traffic —
// the pool's ceiling makes sure every sandbox has one.
func (o *Orchestrator) idleRule() *warm.IdleReaper {
	return warm.NewIdleReaper(o.warm, func(pod *corev1.Pod) time.Duration {
		var spec sandbox.Request
		o.warm.Spec(pod, &spec)
		return time.Duration(spec.IdleTimeoutSeconds) * time.Second
	}, func(ctx context.Context, _, sandboxID string) error {
		return o.Delete(ctx, sandboxID)
	})
}

// onLeadership records leadership transitions when metrics are wired.
func (o *Orchestrator) onLeadership(ctx context.Context, identity string, leading bool) {
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.RecordLeadership(ctx, identity, leading)
	}
}

// Pools reports the configured sandbox pools with live warm/claimed counts.
func (o *Orchestrator) Pools(ctx context.Context) ([]pool.Status, error) {
	pools := o.warm.Pools()
	statuses := make([]pool.Status, 0, len(pools))
	for i := range pools {
		p := &pools[i]
		pods, err := o.warm.Pods(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		w, c := o.warm.Counts(pods)
		statuses = append(statuses, pool.Status{ID: p.ID, Image: p.Image, Size: p.Size, Warm: w, Claimed: c})
	}
	return statuses, nil
}

// Create claims a warm pod for the sandbox, stamps it with the sandbox id and
// its capability token, and waits for the image's contract to answer.
func (o *Orchestrator) Create(ctx context.Context, req *sandbox.Request) (*sandbox.Status, error) {
	p := o.warm.Pool(req.Pool)
	if p == nil {
		return nil, apperrors.NotFound("pool", req.Pool)
	}
	if existing, err := o.warm.Claimed(ctx, req.ID); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return nil, apperrors.Conflict("sandbox", req.ID, "sandbox "+req.ID+" already exists")
	}

	pod, err := o.warm.Claim(ctx, p, claimRequest(p, req))
	if err != nil {
		var poison *claim.Poison
		if errors.As(err, &poison) {
			// Artifact materialization failed: the pod is poisoned (discarded by
			// the control loop, never handed to another sandbox) and this
			// sandbox has failed. No URL — nothing is serving.
			return &sandbox.Status{
				ID: req.ID, PoolID: req.Pool,
				State: sandbox.StateFailed, Error: poison.Msg,
			}, nil
		}
		return nil, err
	}
	if err := o.warm.Bind(ctx, pod.Name, req.ID, req, map[string]string{LabelToken: req.Token}); err != nil {
		return nil, err
	}
	return o.awaitServing(ctx, p, req, pod)
}

// claimRequest maps the sandbox onto the sidecar claim protocol. The command
// falls back from the request to the pool to the installed agent — so the usual
// case is that nobody names one, and the image serves the contract by running
// the agent the shim dropped in its workspace.
func claimRequest(p *pool.Pool, req *sandbox.Request) *proxy.ClaimRequest {
	return &proxy.ClaimRequest{
		ActivationID:   req.ID,
		Command:        cmp.Or(req.Command, p.Command, agentPath),
		Environment:    req.Environment,
		Artifacts:      req.Artifacts,
		Port:           p.Port,
		Ports:          req.Ports,
		TimeoutSeconds: req.TimeoutSeconds,
	}
}

// awaitServing waits for the sandbox's sidecar to report the workload serving,
// so a 201 means the URL is live. Never ready in time → the sandbox is torn
// down and reported failed.
func (o *Orchestrator) awaitServing(ctx context.Context, p *pool.Pool, req *sandbox.Request, pod *corev1.Pod) (*sandbox.Status, error) {
	status := &sandbox.Status{
		ID:     req.ID,
		PoolID: req.Pool,
		URL:    o.cfg.URLFor(req.Token),
		URLs:   o.cfg.URLsFor(req.Token, p.Port, req.Ports),
	}
	deadline := time.Now().Add(o.serveWait)
	for !o.warm.Sidecar().Ready(ctx, pod.Status.PodIP) {
		if time.Now().After(deadline) {
			_ = o.warm.Delete(ctx, pod.Name)
			return &sandbox.Status{
				ID: req.ID, PoolID: req.Pool, State: sandbox.StateFailed,
				Error: fmt.Sprintf("sandbox not serving within %s", o.serveWait),
			}, nil
		}
		if err := o.sleep(ctx); err != nil {
			return nil, err
		}
	}
	status.State = sandbox.StateReady
	return status, nil
}

// Status returns one sandbox's state, derived from its claimed pod.
func (o *Orchestrator) Status(ctx context.Context, id string) (*sandbox.Status, error) {
	pods, err := o.warm.Claimed(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, apperrors.NotFound("sandbox", id)
	}
	status := o.statusFromPod(&pods[0])
	return &status, nil
}

// List returns every live sandbox — one per claimed pod.
func (o *Orchestrator) List(ctx context.Context) ([]sandbox.Status, error) {
	pods, err := o.warm.EveryClaimed(ctx)
	if err != nil {
		return nil, err
	}
	statuses := make([]sandbox.Status, 0, len(pods))
	for i := range pods {
		statuses = append(statuses, o.statusFromPod(&pods[i]))
	}
	return statuses, nil
}

// statusFromPod reconstructs a sandbox from its claimed pod: the labels carry
// the id and the token its URL is built from, the container state the phase —
// creating until the contract answers, then ready. A workload exit or infra
// failure → failed; deletion in flight → deleting.
func (o *Orchestrator) statusFromPod(pod *corev1.Pod) sandbox.Status {
	token := pod.Labels[LabelToken]
	status := sandbox.Status{
		ID:     o.warm.ClaimID(pod),
		PoolID: o.warm.PoolID(pod),
		URL:    o.cfg.URLFor(token),
	}
	// The extra ports come off the stored spec, the primary off the pool — so a
	// reconstructed sandbox advertises exactly the addresses it was created with.
	var spec sandbox.Request
	o.warm.Spec(pod, &spec)
	if p := o.warm.Pool(status.PoolID); p != nil {
		status.URLs = o.cfg.URLsFor(token, p.Port, spec.Ports)
	}
	if pod.DeletionTimestamp != nil {
		status.State = sandbox.StateDeleting
		return status
	}
	if t := warm.WorkloadTerminated(pod); t != nil {
		// A sandbox's workload has no business exiting — that is a failure.
		status.State = sandbox.StateFailed
		status.Error = fmt.Sprintf("sandbox exited with code %d", t.ExitCode)
		return status
	}
	switch {
	case pod.Status.Phase == corev1.PodFailed:
		status.State = sandbox.StateFailed
		status.Error = cmp.Or(pod.Status.Message, pod.Status.Reason)
	case warm.PodReady(pod):
		status.State = sandbox.StateReady
	default:
		status.State = sandbox.StateCreating
	}
	return status
}

// Delete tears the sandbox down by deleting its pod — which also destroys the
// only copy of its capability token, so a leaked URL dies with it. The slot is
// replenished by the control loop, off this path.
func (o *Orchestrator) Delete(ctx context.Context, id string) error {
	pods, err := o.warm.Claimed(ctx, id)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return apperrors.NotFound("sandbox", id)
	}
	for i := range pods {
		if err := o.warm.Delete(ctx, pods[i].Name); err != nil {
			return err
		}
	}
	return nil
}

// Ready checks that the K8s API server is reachable.
func (o *Orchestrator) Ready(ctx context.Context) error { return o.warm.Ready(ctx) }

// Close stops the control loop. Warm and claimed pods are NOT touched —
// Kubernetes keeps them independently and a restart reconciles.
func (o *Orchestrator) Close() error {
	if o.stop != nil {
		o.stop()
	}
	return nil
}

// sleep waits one poll interval, aborting with the context.
func (o *Orchestrator) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(o.poll):
		return nil
	}
}

// Verify Orchestrator implements sandbox.Orchestrator.
var _ sandbox.Orchestrator = (*Orchestrator)(nil)
