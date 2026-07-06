// Package kubernetes implements the pool.Orchestrator interface using the
// Kubernetes API. A warm pool is a fleet of generic pods (pool image + shim
// + claiming sidecar) kept idle; an activation late-binds a payload onto one
// — claim + inject + exec instead of schedule + pull + start. Kubernetes is
// the source of truth: a claimed pod carries its activation ID as a label
// and its spec as an annotation, so Status, List, and a service restart
// reconstruct everything by listing pods. See docs/pools.md.
package kubernetes

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/kube"
	"orchestrator/internal/pool/claim"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/pool"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

const (
	// defaultExecTimeoutSeconds bounds an exec activation that declares no
	// TimeoutSeconds (matches the service default).
	defaultExecTimeoutSeconds = 300
	// maxOutputBytes caps the exec output read back from pod logs (1 MiB).
	maxOutputBytes = 1 << 20
	// controlTick is the leader-elected control loop cadence.
	controlTick = 2 * time.Second

	defaultPoll  = 500 * time.Millisecond
	coldWarmWait = 120 * time.Second // burst-cold: bound on the new pod turning warm-ready
	servingWait  = 60 * time.Second  // HTTP: bound on the claimed workload turning serving-ready
)

// Orchestrator implements pool.Orchestrator using Kubernetes.
type Orchestrator struct {
	client    kubernetes.Interface
	gateway   gatewayclient.Interface
	namespace string
	cfg       Config
	pools     map[string]*pool.Pool // by ID, pointing into cfg.Pools
	claims    claimClient
	stop      context.CancelFunc

	// installKey is the HMAC key claim tokens derive from (token.go),
	// get-or-created as the pool-claim-key Secret and cached here.
	keyMu      sync.Mutex
	installKey []byte

	// Polling knobs, shrunk by unit tests.
	poll      time.Duration
	coldWait  time.Duration
	serveWait time.Duration
}

// NewOrchestrator creates a Kubernetes pool orchestrator.
func NewOrchestrator(ctx context.Context, cfg Config) (*Orchestrator, error) {
	cfg.applyDefaults()
	cs, err := kube.NewClient(cfg.Kubeconfig, cfg.Context, nil)
	if err != nil {
		return nil, err
	}
	gw, err := kube.NewGatewayClient(cfg.Kubeconfig, cfg.Context)
	if err != nil {
		return nil, err
	}
	return wireOrchestrator(cs, gw, cfg), nil
}

// newOrchestrator wires an Orchestrator around injected clients (tests pass
// fakes). cfg must already have defaults applied.
func wireOrchestrator(cs kubernetes.Interface, gw gatewayclient.Interface, cfg Config) *Orchestrator {
	o := &Orchestrator{
		client:    cs,
		gateway:   gw,
		namespace: cfg.Namespace,
		cfg:       cfg,
		claims:    newClaimClient(),
		poll:      defaultPoll,
		coldWait:  coldWarmWait,
		serveWait: servingWait,
	}
	o.pools = make(map[string]*pool.Pool, len(o.cfg.Pools))
	for i := range o.cfg.Pools {
		o.pools[o.cfg.Pools[i].ID] = &o.cfg.Pools[i]
	}
	return o
}

// Start surveys the pre-existing warm/claimed pods (the backend state a
// restart reconstructs), then launches the leader-elected control loop:
// replenishment, poison/orphan GC, idle teardown, and retention GC.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.checkRuntimeClasses(ctx); err != nil {
		return err
	}
	if _, err := o.claimKey(ctx); err != nil {
		return err
	}
	statuses, err := o.Pools(ctx)
	if err != nil {
		return err
	}
	for _, s := range statuses {
		slog.Info("Pool reconciled", "pool", s.ID, "size", s.Size, "warm", s.Warm, "claimed", s.Claimed)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.stop = cancel
	go kube.RunLeaderElected(runCtx, o.client, o.namespace, o.cfg.LeaderElection, o.runControl, nil)
	return nil
}

// checkRuntimeClasses verifies every pool's sandbox RuntimeClass is installed
// — a pool's sandbox is operator config, so a missing class fails Start
// loudly instead of stranding warm pods Pending.
func (o *Orchestrator) checkRuntimeClasses(ctx context.Context) error {
	for i := range o.cfg.Pools {
		p := &o.cfg.Pools[i]
		rc := kube.RuntimeClassFor(o.cfg.SandboxRuntimeClasses, p.Sandbox)
		if rc == "" {
			continue
		}
		_, err := o.client.NodeV1().RuntimeClasses().Get(ctx, rc, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pool %q: RuntimeClass %q (sandbox %q) is not installed", p.ID, rc, p.Sandbox)
		}
		if err != nil {
			return apperrors.Internal("kubernetes.getRuntimeClass", err)
		}
	}
	return nil
}

// Pools reports the configured pools with live warm/claimed counts. Warm
// counts only warm-READY pods (kubelet-probed sidecar /ready), matching the
// claimable set; pods still starting are neither warm nor claimed.
func (o *Orchestrator) Pools(ctx context.Context) ([]pool.Status, error) {
	statuses := make([]pool.Status, 0, len(o.cfg.Pools))
	for i := range o.cfg.Pools {
		p := &o.cfg.Pools[i]
		pods, err := o.poolPods(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		s := pool.Status{ID: p.ID, Image: p.Image, Size: p.Size}
		for j := range pods {
			switch {
			case pods[j].Labels[LabelActivation] != "":
				s.Claimed++
			case claimable(&pods[j]):
				s.Warm++
			}
		}
		statuses = append(statuses, s)
	}
	return statuses, nil
}

// Activate claims a warm pod and late-binds the activation onto it: claim
// POST (racing losers get 409 and retry the next pod), then the pod is
// labeled with the activation and annotated with its spec. Exec pools block
// until the workload exits; HTTP pools expose a Service + HTTPRoute and
// return once serving.
func (o *Orchestrator) Activate(ctx context.Context, poolID string, act *pool.Activation) (*pool.ActivationStatus, error) {
	p, ok := o.pools[poolID]
	if !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	if act.ID == "" {
		id, err := randHex(6)
		if err != nil {
			return nil, apperrors.Internal("kubernetes.generateID", err)
		}
		act.ID = id
	}
	if existing, err := o.activationPods(ctx, poolID, act.ID); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return nil, apperrors.Conflict("activation", act.ID, "activation "+act.ID+" already exists")
	}

	pod, err := o.claimWarmPod(ctx, p, act)
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
	if err := o.bindPod(ctx, pod.Name, act); err != nil {
		return nil, err
	}
	if !p.HTTP() {
		return o.awaitExec(ctx, p, act, pod.Name)
	}
	return o.exposeHTTP(ctx, p, act, pod)
}

// claimWarmPod wins a free warm pod via the shared claim flow — the pod is
// the serialization point, so the service stays stateless. The bearer token
// is derived from the pod name (token.go), not stored anywhere.
func (o *Orchestrator) claimWarmPod(ctx context.Context, p *pool.Pool, act *pool.Activation) (*corev1.Pod, error) {
	key, err := o.claimKey(ctx)
	if err != nil {
		return nil, err
	}
	inv := &podInventory{o: o, p: p, key: key, byName: make(map[string]*corev1.Pod)}
	unit, err := claim.Claim(ctx, inv, clientPoster{o.claims}, p.ID, p.Burst, claimRequest(p, act))
	if err != nil {
		return nil, err
	}
	return inv.byName[unit.ID], nil
}

// podInventory is the Kubernetes warm-unit surface behind the claim flow's
// seam: free units are claimable pool pods, a cold create pays the burst
// cold start. Pods are cached by name so the winner's object is at hand
// without re-fetching.
type podInventory struct {
	o      *Orchestrator
	p      *pool.Pool
	key    []byte
	byName map[string]*corev1.Pod
}

func (inv *podInventory) Free(ctx context.Context) ([]claim.Unit, error) {
	pods, err := inv.o.poolPods(ctx, inv.p.ID)
	if err != nil {
		return nil, err
	}
	var units []claim.Unit
	for i := range pods {
		pod := &pods[i]
		if !claimable(pod) {
			continue
		}
		inv.byName[pod.Name] = pod
		units = append(units, inv.unitFor(pod))
	}
	return units, nil
}

// Create creates a pod and waits for it to turn warm-ready (bounded). A
// pod that never warms is deleted so the burst does not leak capacity beyond
// the pool size.
func (inv *podInventory) Create(ctx context.Context) (*claim.Unit, error) {
	created, err := inv.o.createWarmPod(ctx, inv.p)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(inv.o.coldWait)
	for {
		pod, err := inv.o.client.CoreV1().Pods(inv.o.namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return nil, apperrors.Internal("kubernetes.getPod", err)
		}
		if claimable(pod) {
			inv.byName[pod.Name] = pod
			unit := inv.unitFor(pod)
			return &unit, nil
		}
		if time.Now().After(deadline) {
			_ = inv.o.deletePod(ctx, created.Name)
			return nil, apperrors.Internal("kubernetes.coldClaim",
				fmt.Errorf("cold pod %s not warm-ready within %s", created.Name, inv.o.coldWait))
		}
		if err := inv.o.sleep(ctx); err != nil {
			return nil, err
		}
	}
}

func (inv *podInventory) unitFor(pod *corev1.Pod) claim.Unit {
	return claim.Unit{
		ID:    pod.Name,
		Addr:  pod.Status.PodIP,
		Token: deriveClaimToken(inv.key, pod.Name),
	}
}

// claimable reports whether a pod is in the free warm set: unclaimed, not
// being deleted, and warm-ready (the kubelet-probed sidecar /ready gate,
// surfaced as the pod Ready condition).
func claimable(pod *corev1.Pod) bool {
	return pod.Labels[LabelActivation] == "" &&
		pod.DeletionTimestamp == nil &&
		pod.Status.PodIP != "" &&
		isPodReady(pod)
}

// claimRequest maps the activation onto the sidecar claim protocol.
func claimRequest(p *pool.Pool, act *pool.Activation) *proxy.ClaimRequest {
	return &proxy.ClaimRequest{
		ActivationID:   act.ID,
		Command:        act.Command,
		Environment:    act.Environment,
		Artifacts:      act.Artifacts,
		Port:           p.Port,
		TimeoutSeconds: act.TimeoutSeconds,
	}
}

// bindPod stamps the accepted claim onto the pod: the activation label (the
// Status/List/GC key) and the spec annotation (Status reconstruction). The
// callback signing key is intentionally STRIPPED from the annotation — the
// full callback lives only in the in-flight request (sync and async both
// complete within the service process; Status/List never need the key), so
// no secret material rests on the pod object. A restart-reconstructed
// activation therefore cannot deliver callbacks — the documented
// at-most-once semantics.
func (o *Orchestrator) bindPod(ctx context.Context, podName string, act *pool.Activation) error {
	redacted := *act
	if act.Callback != nil {
		cb := *act.Callback
		cb.Key = ""
		redacted.Callback = &cb
	}
	spec, err := json.Marshal(&redacted)
	if err != nil {
		return apperrors.Internal("kubernetes.marshalActivation", err)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":      map[string]string{LabelActivation: act.ID},
		"annotations": map[string]string{AnnotationActivationSpec: string(spec)},
	}})
	if err != nil {
		return apperrors.Internal("kubernetes.marshalPatch", err)
	}
	// A crash before this patch leaves a claimed-but-unlabeled pod; orphan GC
	// discards it after OrphanTTL — orphans are garbage, never resold.
	if _, err := o.client.CoreV1().Pods(o.namespace).Patch(ctx, podName, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return apperrors.Internal("kubernetes.bindPod", err)
	}
	return nil
}

// awaitExec blocks until the workload container terminates — returning its
// exit code and captured output — or the activation timeout elapses, in
// which case the pod is discarded and the activation reported failed. The
// finished pod is kept for Status until retention GC reaps it.
func (o *Orchestrator) awaitExec(ctx context.Context, p *pool.Pool, act *pool.Activation, podName string) (*pool.ActivationStatus, error) {
	status := &pool.ActivationStatus{ID: act.ID, PoolID: p.ID, PodID: podName}
	timeout := time.Duration(cmp.Or(act.TimeoutSeconds, defaultExecTimeoutSeconds)) * time.Second
	deadline := time.Now().Add(timeout)
	for {
		pod, err := o.client.CoreV1().Pods(o.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, apperrors.Internal("kubernetes.getPod", err)
		}
		if t := workloadTerminated(pod); t != nil {
			code := int(t.ExitCode)
			status.State = pool.StateExited
			status.ExitCode = &code
			status.Output = o.workloadOutput(ctx, podName)
			return status, nil
		}
		if time.Now().After(deadline) {
			_ = o.deletePod(ctx, podName)
			status.State = pool.StateFailed
			status.Error = "timeout"
			return status, nil
		}
		if err := o.sleep(ctx); err != nil {
			return nil, err
		}
	}
}

// workloadOutput reads the workload container's logs, capped at 1 MiB.
func (o *Orchestrator) workloadOutput(ctx context.Context, podName string) string {
	limit := int64(maxOutputBytes)
	raw, err := o.client.CoreV1().Pods(o.namespace).
		GetLogs(podName, &corev1.PodLogOptions{Container: ContainerWorkload, LimitBytes: &limit}).
		DoRaw(ctx)
	if err != nil {
		slog.Warn("Reading workload output failed", "pod", podName, "error", err)
		return ""
	}
	return string(raw)
}

// exposeHTTP publishes the claimed pod at its gateway URL — a per-activation
// Service (selecting the activation label) plus HTTPRoute — then waits for
// the workload to turn serving-ready. Never ready in time → the activation
// is torn down and reported failed (failure-semantics.md).
func (o *Orchestrator) exposeHTTP(ctx context.Context, p *pool.Pool, act *pool.Activation, pod *corev1.Pod) (*pool.ActivationStatus, error) {
	host := activationHost(act.Host, act.ID, o.cfg.PoolDomain)
	if err := o.createActivationService(ctx, p.ID, act.ID); err != nil {
		return nil, err
	}
	if err := o.createActivationRoute(ctx, p.ID, act.ID, host); err != nil {
		return nil, err
	}
	status := &pool.ActivationStatus{ID: act.ID, PoolID: p.ID, PodID: pod.Name, URL: "http://" + host}
	deadline := time.Now().Add(o.serveWait)
	for !o.claims.Ready(ctx, pod.Status.PodIP) {
		if time.Now().After(deadline) {
			_ = o.deleteActivationObjects(ctx, act.ID)
			_ = o.deletePod(ctx, pod.Name)
			status.State = pool.StateFailed
			status.Error = fmt.Sprintf("workload not serving within %s", o.serveWait)
			return status, nil
		}
		if err := o.sleep(ctx); err != nil {
			return nil, err
		}
	}
	status.State = pool.StateReady
	return status, nil
}

// Status returns one activation's state, derived from its claimed pod.
func (o *Orchestrator) Status(ctx context.Context, poolID, activationID string) (*pool.ActivationStatus, error) {
	p, ok := o.pools[poolID]
	if !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	pods, err := o.activationPods(ctx, poolID, activationID)
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
	p, ok := o.pools[poolID]
	if !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	list, err := o.client.CoreV1().Pods(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue + "," + LabelPoolID + "=" + poolID + "," + LabelActivation,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listPods", err)
	}
	statuses := make([]pool.ActivationStatus, 0, len(list.Items))
	for i := range list.Items {
		statuses = append(statuses, o.statusFromPod(p, &list.Items[i]))
	}
	return statuses, nil
}

// statusFromPod reconstructs an activation's status from its claimed pod:
// the label carries the ID, the annotation the original spec, the container
// state the phase. Exec pods report ready while the workload runs and exited
// with its code after; HTTP pods report activating until serving-ready, then
// ready. Infra failure → failed; deletion in flight → deactivating.
func (o *Orchestrator) statusFromPod(p *pool.Pool, pod *corev1.Pod) pool.ActivationStatus {
	status := pool.ActivationStatus{
		ID:     pod.Labels[LabelActivation],
		PoolID: p.ID,
		PodID:  pod.Name,
	}
	var act pool.Activation
	_ = json.Unmarshal([]byte(pod.Annotations[AnnotationActivationSpec]), &act)
	if p.HTTP() {
		status.URL = "http://" + activationHost(act.Host, status.ID, o.cfg.PoolDomain)
	}

	if pod.DeletionTimestamp != nil {
		status.State = pool.StateDeactivating
		return status
	}
	if t := workloadTerminated(pod); t != nil {
		if p.HTTP() {
			// An HTTP workload has no business exiting — that is a failure.
			status.State = pool.StateFailed
			status.Error = fmt.Sprintf("workload exited with code %d", t.ExitCode)
		} else {
			code := int(t.ExitCode)
			status.State = pool.StateExited
			status.ExitCode = &code
		}
		return status
	}
	switch {
	case pod.Status.Phase == corev1.PodFailed:
		status.State = pool.StateFailed
		status.Error = cmp.Or(pod.Status.Message, pod.Status.Reason)
	case !p.HTTP() && pod.Status.Phase == corev1.PodRunning:
		status.State = pool.StateReady
	case p.HTTP() && isPodReady(pod):
		status.State = pool.StateReady
	default:
		status.State = pool.StateActivating
	}
	return status
}

// Deactivate tears the activation down: its route and Service (HTTP), then
// the pod — last, so a crashed teardown is still visible and retryable. The
// slot is replenished by the control loop, off this path.
func (o *Orchestrator) Deactivate(ctx context.Context, poolID, activationID string) error {
	if _, ok := o.pools[poolID]; !ok {
		return apperrors.NotFound("pool", poolID)
	}
	pods, err := o.activationPods(ctx, poolID, activationID)
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
		if err := o.deletePod(ctx, pods[i].Name); err != nil {
			return err
		}
	}
	return nil
}

// Ready checks that the K8s API server is reachable.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Discovery().ServerVersion()
	return err
}

// Close stops the control loop. Warm and claimed pods are NOT touched —
// Kubernetes keeps them independently and a restart reconciles.
func (o *Orchestrator) Close() error {
	if o.stop != nil {
		o.stop()
	}
	return nil
}

// poolPods lists a pool's managed pods.
func (o *Orchestrator) poolPods(ctx context.Context, poolID string) ([]corev1.Pod, error) {
	list, err := o.client.CoreV1().Pods(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue + "," + LabelPoolID + "=" + poolID,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listPods", err)
	}
	return list.Items, nil
}

// activationPods lists the pod(s) bound to one activation.
func (o *Orchestrator) activationPods(ctx context.Context, poolID, activationID string) ([]corev1.Pod, error) {
	list, err := o.client.CoreV1().Pods(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue + "," + LabelPoolID + "=" + poolID + "," + LabelActivation + "=" + activationID,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listPods", err)
	}
	return list.Items, nil
}

// createWarmPod creates one warm pod. The name is chosen client-side (not
// GenerateName) so the claim token can be derived from it before creation.
func (o *Orchestrator) createWarmPod(ctx context.Context, p *pool.Pool) (*corev1.Pod, error) {
	key, err := o.claimKey(ctx)
	if err != nil {
		return nil, err
	}
	suffix, err := randHex(5)
	if err != nil {
		return nil, apperrors.Internal("kubernetes.podName", err)
	}
	name := "pool-" + p.ID + "-" + suffix
	created, err := o.client.CoreV1().Pods(o.namespace).Create(ctx, buildWarmPod(p, o.cfg, name, deriveClaimToken(key, name)), metav1.CreateOptions{})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.createPod", err)
	}
	return created, nil
}

// deletePod removes a pod, tolerating already-gone.
func (o *Orchestrator) deletePod(ctx context.Context, name string) error {
	err := o.client.CoreV1().Pods(o.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deletePod", err)
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

// workloadTerminated returns the workload container's terminated state, or
// nil while it runs (or before it starts).
func workloadTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == ContainerWorkload {
			return pod.Status.ContainerStatuses[i].State.Terminated
		}
	}
	return nil
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Verify Orchestrator implements pool.Orchestrator.
var _ pool.Orchestrator = (*Orchestrator)(nil)
