// Package kubernetes implements the pool.Orchestrator interface using the
// Kubernetes API. The warm pods, the claim protocol, the serving wait, the
// phase rule, and the control loop all belong to internal/warm; what this
// package adds is what an ACTIVATION is on top of a claimed pod — a
// per-activation Service and HTTPRoute, and the vocabulary its phases are
// published in. Kubernetes stays the source of truth: a claimed pod carries
// its activation ID as a label and its spec as an annotation, so Status, List,
// and a service restart reconstruct everything by listing pods.
// See docs/pools.md.
package kubernetes

import (
	"cmp"
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/kube"
	"orchestrator/internal/pool"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// Label and annotation contract for pool objects.
const (
	LabelManagedBy  = warm.LabelManagedBy
	LabelPoolID     = "pool.id"
	LabelActivation = "pool.activation"
	ManagedByValue  = "deployments-service"

	// AnnotationActivationSpec carries the accepted pool.Activation JSON on
	// claimed pods — the Status reconstruction source. The callback signing
	// key is stripped before writing; claim tokens are derived, never
	// annotated.
	AnnotationActivationSpec = "pool.activation-spec"
)

// naming is this consumer's label and pod-name contract. Pool pods are not
// sandbox pods: separate keys keep each consumer's pods visible to exactly one
// control loop.
func naming() warm.Naming {
	return warm.Naming{
		ManagedBy:  ManagedByValue,
		Kind:       "pool",
		Pool:       LabelPoolID,
		Claim:      LabelActivation,
		Spec:       AnnotationActivationSpec,
		NamePrefix: "pool",
		SecretName: "pool-claim-key",
	}
}

// Orchestrator implements pool.Orchestrator using Kubernetes.
type Orchestrator struct {
	client  kubernetes.Interface
	gateway gatewayclient.Interface
	warm    *warm.Manager
	cfg     Config
}

// NewOrchestrator creates a Kubernetes pool orchestrator.
func NewOrchestrator(ctx context.Context, cfg Config) (*Orchestrator, error) {
	cfg.applyDefaults()
	cs, err := kube.NewClient(cfg.Kubeconfig, cfg.Context, cfg.Metrics)
	if err != nil {
		return nil, err
	}
	gw, err := kube.NewGatewayClient(cfg.Kubeconfig, cfg.Context)
	if err != nil {
		return nil, err
	}
	return wireOrchestrator(cs, gw, cfg, nil), nil
}

// wireOrchestrator wires an Orchestrator around injected clients (tests pass
// fakes, and tune the warm layer: a fake sidecar client, since fake-clientset
// pods have no reachable IPs, and shorter waits). cfg must already have
// defaults applied.
func wireOrchestrator(cs kubernetes.Interface, gw gatewayclient.Interface, cfg Config, tune func(*warm.Config)) *Orchestrator {
	warmCfg := cfg.warmConfig()
	if tune != nil {
		tune(&warmCfg)
	}
	return &Orchestrator{
		client:  cs,
		gateway: gw,
		warm:    warm.New(cs, cfg.Pools, warmCfg),
		cfg:     cfg,
	}
}

// Start brings the warm layer up: pool verification, the restart survey, and
// the leader-elected control loop, whose idle rule deactivates an activation
// that goes quiet for its window.
func (o *Orchestrator) Start(ctx context.Context) error {
	return o.warm.Run(ctx, o.Deactivate)
}

// Pools reports the configured pools with live warm/claimed counts. Warm
// counts only warm-READY pods (kubelet-probed sidecar /ready), matching the
// claimable set; pods still starting are neither warm nor claimed.
func (o *Orchestrator) Pools(ctx context.Context) ([]pool.Status, error) {
	return o.warm.PoolStatuses(ctx)
}

// Activate claims a warm pod and late-binds the activation onto it: claim
// POST (racing losers get 409 and retry the next pod), then the pod is labeled
// with the activation and annotated with its spec, published at its gateway
// URL, and awaited until serving.
func (o *Orchestrator) Activate(ctx context.Context, poolID string, act *pool.Activation) (*pool.ActivationStatus, error) {
	p := o.warm.Pool(poolID)
	if p == nil {
		return nil, apperrors.NotFound("pool", poolID)
	}
	if act.ID == "" {
		id, err := warm.RandHex(6)
		if err != nil {
			return nil, apperrors.Internal("kubernetes.generateID", err)
		}
		act.ID = id
	}
	if existing, err := o.warm.Claimed(ctx, poolID, act.ID); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return nil, apperrors.Conflict("activation", act.ID, "activation "+act.ID+" already exists")
	}

	pod, err := o.warm.Claim(ctx, p, claimRequest(p, act))
	if err != nil {
		var poison *claim.Poison
		if errors.As(err, &poison) {
			return &pool.ActivationStatus{
				ID: act.ID, PoolID: poolID, PodID: poison.Unit,
				State: pool.StateFailed, Error: poison.Msg,
			}, nil
		}
		return nil, err
	}
	// The callback signing key is deliberately STRIPPED from the stored spec —
	// the full callback lives only in the in-flight request (sync and async
	// both complete within the service process; Status/List never need the
	// key), so no secret material rests on the pod object. A
	// restart-reconstructed activation therefore cannot deliver callbacks: the
	// documented at-most-once semantics.
	if err := o.warm.Bind(ctx, pod.Name, act.ID, redacted(act), nil); err != nil {
		return nil, err
	}
	return o.exposeHTTP(ctx, p, act, pod)
}

// redacted copies the activation without its callback signing key.
func redacted(act *pool.Activation) *pool.Activation {
	out := *act
	if act.Callback != nil {
		cb := *act.Callback
		cb.Key = ""
		out.Callback = &cb
	}
	return &out
}

// claimRequest maps the activation onto the sidecar claim protocol; a request
// without a command falls back to the pool's.
func claimRequest(p *pool.Pool, act *pool.Activation) *workload.ClaimRequest {
	return &workload.ClaimRequest{
		ActivationID:   act.ID,
		Command:        cmp.Or(act.Command, p.Command),
		Environment:    act.Environment,
		Artifacts:      act.Artifacts,
		Port:           p.Port,
		TimeoutSeconds: &act.TimeoutSeconds,
	}
}

// exposeHTTP publishes the claimed pod at its gateway URL — a per-activation
// Service (selecting the activation label) plus HTTPRoute — then waits for the
// workload to turn serving-ready. Never ready in time → the activation is torn
// down and reported failed (failure-semantics.md).
func (o *Orchestrator) exposeHTTP(ctx context.Context, p *pool.Pool, act *pool.Activation, pod *corev1.Pod) (*pool.ActivationStatus, error) {
	host := activationHost(act.Host, act.ID, o.cfg.PoolDomain)
	if err := o.createActivationService(ctx, p.ID, act.ID); err != nil {
		return nil, err
	}
	if err := o.createActivationRoute(ctx, p.ID, act.ID, host); err != nil {
		return nil, err
	}
	status := &pool.ActivationStatus{ID: act.ID, PoolID: p.ID, PodID: pod.Name, URL: "http://" + host}
	unserved, err := o.warm.Await(ctx, pod)
	if err != nil {
		return nil, err
	}
	if unserved != "" {
		// warm deleted the pod; the route and Service are ours to remove.
		_ = o.deleteActivationObjects(ctx, act.ID)
		status.State = pool.StateFailed
		status.Error = unserved
		return status, nil
	}
	status.State = pool.StateReady
	return status, nil
}

// Status returns one activation's state, derived from its claimed pod.
func (o *Orchestrator) Status(ctx context.Context, poolID, activationID string) (*pool.ActivationStatus, error) {
	p := o.warm.Pool(poolID)
	if p == nil {
		return nil, apperrors.NotFound("pool", poolID)
	}
	pods, err := o.warm.Claimed(ctx, poolID, activationID)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, apperrors.NotFound("activation", activationID)
	}
	status := o.statusFromPod(p, &pods[0])
	return &status, nil
}

// List returns the pool's live activations — every claimed pod.
func (o *Orchestrator) List(ctx context.Context, poolID string) ([]pool.ActivationStatus, error) {
	p := o.warm.Pool(poolID)
	if p == nil {
		return nil, apperrors.NotFound("pool", poolID)
	}
	pods, err := o.warm.Claimed(ctx, poolID, "")
	if err != nil {
		return nil, err
	}
	statuses := make([]pool.ActivationStatus, 0, len(pods))
	for i := range pods {
		statuses = append(statuses, o.statusFromPod(p, &pods[i]))
	}
	return statuses, nil
}

// statusFromPod reconstructs an activation's status from its claimed pod: the
// label carries the ID, the annotation the original spec, and warm derives the
// phase — all this package adds is the activation's URL and its own vocabulary.
func (o *Orchestrator) statusFromPod(p *pool.Pool, pod *corev1.Pod) pool.ActivationStatus {
	obs := o.warm.Observe(pod)
	var act pool.Activation
	o.warm.Spec(pod, &act)
	return pool.ActivationStatus{
		ID:     obs.ClaimID,
		PoolID: p.ID,
		PodID:  obs.PodName,
		URL:    "http://" + activationHost(act.Host, obs.ClaimID, o.cfg.PoolDomain),
		State:  activationState(obs.Phase),
		Error:  obs.Error,
	}
}

// activationState names a phase in the activation vocabulary.
func activationState(phase warm.Phase) string {
	switch phase {
	case warm.PhaseServing:
		return pool.StateReady
	case warm.PhaseFailed:
		return pool.StateFailed
	case warm.PhaseTerminating:
		return pool.StateDeactivating
	case warm.PhaseStarting:
	}
	return pool.StateActivating
}

// Deactivate tears the activation down: its route and Service, then the pod —
// last, so a crashed teardown is still visible and retryable. The slot is
// replenished by the control loop, off this path.
func (o *Orchestrator) Deactivate(ctx context.Context, poolID, activationID string) error {
	if o.warm.Pool(poolID) == nil {
		return apperrors.NotFound("pool", poolID)
	}
	pods, err := o.warm.Claimed(ctx, poolID, activationID)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return apperrors.NotFound("activation", activationID)
	}
	if err := o.deleteActivationObjects(ctx, activationID); err != nil {
		return err
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

// Close stops the control loop, leaving the pods to Kubernetes.
func (o *Orchestrator) Close() error {
	o.warm.Stop()
	return nil
}

// Verify Orchestrator implements pool.Orchestrator.
var _ pool.Orchestrator = (*Orchestrator)(nil)
