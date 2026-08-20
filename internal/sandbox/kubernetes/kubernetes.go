// Package kubernetes implements the sandbox.Orchestrator interface using the
// Kubernetes API. Everything about standing warm capacity — the pods, the
// claim, the serving wait, replenishment, poison and orphan GC, the idle rule
// — belongs to internal/warm. What a SANDBOX adds on top of a claimed pod is
// small on purpose: a capability token stamped as a label, and the addresses
// that token resolves to. There is no per-sandbox Service or route; the sandbox
// edge resolves the token from the request's Host behind one wildcard route,
// so a create is as fast as the claim and a delete churns nothing.
// See docs/sandboxes.md.
package kubernetes

import (
	"cmp"
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/kube"
	"orchestrator/internal/pool"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
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

	ManagedByValue = "sandbox-service"
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
	addr   sandbox.Addressing
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
		client: cs,
		warm:   warm.New(cs, cfg.Pools, warmCfg),
		cfg:    cfg,
		addr:   cfg.addressing(),
	}
}

// Start brings the warm layer up: pool verification, the restart survey, and
// the leader-elected control loop, whose idle rule deletes a sandbox that goes
// quiet for its window — the pool's ceiling makes sure every sandbox has one.
func (o *Orchestrator) Start(ctx context.Context) error {
	return o.warm.Run(ctx, func(ctx context.Context, _, sandboxID string) error {
		return o.Delete(ctx, sandboxID)
	})
}

// Pools reports the configured sandbox pools with live warm/claimed counts.
func (o *Orchestrator) Pools(ctx context.Context) ([]pool.Status, error) {
	return o.warm.PoolStatuses(ctx)
}

// Create claims a warm pod for the sandbox, stamps it with the sandbox id and
// its capability token, and waits for the image's contract to answer.
func (o *Orchestrator) Create(ctx context.Context, req *sandbox.Request) (*sandbox.Status, error) {
	// A declared pool fixes the shape; a poolless sandbox describes its own.
	shape := req.Shape()
	var p *pool.Pool
	if req.Pool != "" {
		if p = o.warm.Pool(req.Pool); p == nil {
			return nil, apperrors.NotFound("pool", req.Pool)
		}
		shape = p.Spec
	}
	if existing, err := o.warm.Claimed(ctx, "", req.ID); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return nil, apperrors.Conflict("sandbox", req.ID, "sandbox "+req.ID+" already exists")
	}

	// Claim a warm pod, or — with no pool behind this sandbox — create the one
	// pod it needs and claim that. The second path skips the warm pass and the
	// burst policy: there is no standing capacity to scan, and the pod is
	// labeled with the sandbox's own id so it is never offered to another claim.
	var pod *corev1.Pod
	var err error
	if p == nil {
		pod, err = o.warm.CreateClaimed(ctx, &shape, req.ID, claimRequest(&shape, req))
	} else {
		pod, err = o.warm.Claim(ctx, p, claimRequest(&p.Spec, req))
	}
	if err != nil {
		var poison *claim.Poison
		if errors.As(err, &poison) {
			// Artifact materialization failed: the pod is poisoned and this
			// sandbox has failed. No URL — nothing is serving. The pod is
			// discarded either way: a declared pool's control loop drops it, and
			// CreateClaimed drops the one it created.
			return &sandbox.Status{
				ID: req.ID, PoolID: req.Pool,
				State: sandbox.StateFailed, Error: poison.Msg,
			}, nil
		}
		return nil, err
	}
	if err := o.warm.Bind(ctx, pod.Name, req.ID, req, map[string]string{LabelToken: req.Token}); err != nil {
		// The claim label is what makes a pod discoverable. A poolless pod that
		// never got one belongs to no pool the control loop reconciles, so if the
		// bind is what failed — a cancelled request, a lost API server — this is
		// the last place that can reclaim it.
		if p == nil {
			o.warm.Discard(ctx, pod.Name)
		}
		return nil, err
	}
	return o.awaitServing(ctx, &shape, req, pod)
}

// claimRequest maps the sandbox onto the sidecar claim protocol. The command
// falls back from the request to the pool to the installed agent — so the usual
// case is that nobody names one, and the image serves the contract by running
// the agent the shim dropped in its workspace.
func claimRequest(p *pool.Spec, req *sandbox.Request) *workload.ClaimRequest {
	return &workload.ClaimRequest{
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
func (o *Orchestrator) awaitServing(ctx context.Context, p *pool.Spec, req *sandbox.Request, pod *corev1.Pod) (*sandbox.Status, error) {
	status := &sandbox.Status{
		ID:     req.ID,
		PoolID: req.Pool,
		URL:    o.addr.URL(req.Token),
		URLs:   o.addr.URLs(req.Token, p.Port, req.Ports),
	}
	unserved, err := o.warm.Await(ctx, pod)
	if err != nil {
		return nil, err
	}
	if unserved != "" {
		// warm deleted the pod, so the token it carried is gone with it — the
		// failed sandbox has no URL because nothing is serving.
		return &sandbox.Status{
			ID: req.ID, PoolID: req.Pool, State: sandbox.StateFailed, Error: unserved,
		}, nil
	}
	status.State = sandbox.StateReady
	return status, nil
}

// Status returns one sandbox's state, derived from its claimed pod.
func (o *Orchestrator) Status(ctx context.Context, id string) (*sandbox.Status, error) {
	pods, err := o.warm.Claimed(ctx, "", id)
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
	pods, err := o.warm.Claimed(ctx, "", "")
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
// the id and the token its URL is built from, and warm derives the phase — all
// this package adds is the addresses and its own vocabulary.
func (o *Orchestrator) statusFromPod(pod *corev1.Pod) sandbox.Status {
	obs := o.warm.Observe(pod)
	token := pod.Labels[LabelToken]
	// The extra ports come off the stored spec, the primary off the pool — so a
	// reconstructed sandbox advertises exactly the addresses it was created with.
	var spec sandbox.Request
	o.warm.Spec(pod, &spec)
	status := sandbox.Status{
		ID:     obs.ClaimID,
		PoolID: obs.PoolID,
		URL:    o.addr.URL(token),
		State:  sandboxState(obs.Phase),
		Error:  obs.Error,
	}
	if p := o.warm.Pool(status.PoolID); p != nil {
		status.URLs = o.addr.URLs(token, p.Port, spec.Ports)
	}
	return status
}

// sandboxState names a phase in the sandbox vocabulary.
func sandboxState(phase warm.Phase) string {
	switch phase {
	case warm.PhaseServing:
		return sandbox.StateReady
	case warm.PhaseFailed:
		return sandbox.StateFailed
	case warm.PhaseTerminating:
		return sandbox.StateDeleting
	case warm.PhaseStarting:
	}
	return sandbox.StateCreating
}

// Delete tears the sandbox down by deleting its pod — which also destroys the
// only copy of its capability token, so a leaked URL dies with it. The slot is
// replenished by the control loop, off this path.
func (o *Orchestrator) Delete(ctx context.Context, id string) error {
	pods, err := o.warm.Claimed(ctx, "", id)
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

// Close stops the control loop, leaving the pods to Kubernetes.
func (o *Orchestrator) Close() error {
	o.warm.Stop()
	return nil
}

// Verify Orchestrator implements sandbox.Orchestrator.
var _ sandbox.Orchestrator = (*Orchestrator)(nil)
