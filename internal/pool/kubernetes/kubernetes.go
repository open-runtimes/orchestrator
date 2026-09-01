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
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/kube"
	"orchestrator/internal/pool"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"
	"time"

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

// tearDownTimeout bounds cleaning up an activation whose caller is gone.
const tearDownTimeout = 30 * time.Second

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
	restCfg, err := kube.NewConfig(cfg.Kubeconfig, cfg.Context, cfg.Metrics, 0, 0)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	gw, err := gatewayclient.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway client: %w", err)
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
//
// An ERROR from here is torn down too, and for the same reason: the caller never
// received the URL, so nothing it could remove was ever published to it. What
// would otherwise be left is a pod that is claimed and labeled — permanently out
// of the warm set, and collected only where the activation or its pool declared
// an idle timeout — beside a Service and route pointing at it.
func (o *Orchestrator) exposeHTTP(ctx context.Context, p *pool.Pool, act *pool.Activation, pod *corev1.Pod) (_ *pool.ActivationStatus, err error) {
	host := activationHost(act.Host, act.ID, o.cfg.PoolDomain)
	defer func() {
		if err != nil {
			o.tearDown(ctx, p.ID, act.ID, pod.Name)
		}
	}()
	if err = o.createActivationService(ctx, p.ID, act.ID); err != nil {
		return nil, err
	}
	if err = o.createActivationRoute(ctx, p.ID, act.ID, host); err != nil {
		return nil, err
	}
	status := &pool.ActivationStatus{ID: act.ID, PoolID: p.ID, PodID: pod.Name, URL: "http://" + host}
	unserved, err := o.warm.Await(ctx, pod)
	if err != nil {
		return nil, err
	}
	if unserved != "" {
		// warm deleted the pod; the route and Service are ours to remove.
		o.tearDown(ctx, p.ID, act.ID, pod.Name)
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

// tearDown removes an activation that was never handed over: its routing
// objects, if this pod was the last thing claiming them, then the pod itself.
//
// It detaches from the caller's context first. The common way to reach here is a
// client that hung up mid-activation — which cancelled that very context, so
// every delete issued on it would fail too, and the activation would survive the
// request that gave up on it. Failures are logged and the teardown continues:
// each object is worth removing whether or not the last one went.
func (o *Orchestrator) tearDown(ctx context.Context, poolID, activationID, podName string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tearDownTimeout)
	defer cancel()
	if o.lastClaimant(ctx, poolID, activationID, podName) {
		if err := o.deleteActivationObjects(ctx, activationID); err != nil {
			slog.Warn("Failed to remove an unfinished activation's objects", "activationId", activationID, "error", err)
		}
	}
	// Already gone when the serving wait timed out — warm discards the pod
	// itself there — and Delete tolerates that.
	if err := o.warm.Delete(ctx, podName); err != nil {
		slog.Warn("Failed to remove an unfinished activation's pod", "activationId", activationID, "pod", podName, "error", err)
	}
}

// lastClaimant reports whether podName is the only pod left claiming this
// activation id — the condition for removing routing objects, which are keyed by
// that id alone and therefore shared by anything else claiming it. An id CAN
// have a second claimant: the duplicate guard is a read taken before the claim,
// so two concurrent creates both get past it, and the object creates tolerate
// AlreadyExists, so whichever of them published does not matter.
//
// Conservative when it cannot tell. A Service left behind is recoverable — a
// Deactivate removes it, and it serves nothing once its pods are gone — while a
// live activation whose route was deleted under it is not.
func (o *Orchestrator) lastClaimant(ctx context.Context, poolID, activationID, podName string) bool {
	pods, err := o.warm.Claimed(ctx, poolID, activationID)
	if err != nil {
		slog.Warn("Could not tell whether an activation's objects are still in use; leaving them",
			"activationId", activationID, "error", err)
		return false
	}
	for i := range pods {
		if pods[i].Name == podName || pods[i].DeletionTimestamp != nil {
			continue
		}
		slog.Warn("Another claim still uses this activation's route; leaving it published",
			"activationId", activationID, "pod", pods[i].Name)
		return false
	}
	return true
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
