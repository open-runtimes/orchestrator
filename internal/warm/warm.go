// Package warm keeps pools of generic pods (runtime image + shim + claiming
// sidecar) standing idle on Kubernetes, and hands one to a caller on demand:
// claim + inject + exec instead of schedule + pull + start. Kubernetes is the
// source of truth — a claimed pod carries its claim id as a label and its
// spec as an annotation, so status, listing, and a service restart all
// reconstruct by listing pods.
//
// Everything here is consumer-neutral, and that includes the sequence, not
// just the primitives: claiming, binding, waiting for the workload to answer,
// deriving a claimed pod's phase, reaping it when it goes idle, and running the
// inventory control loop. Deployment Revisions and sandboxes both sit on it;
// the owning controller supplies routing and lifecycle once a pod is claimed.
package warm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	"orchestrator/internal/pool"
	"orchestrator/internal/workload"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// LabelManagedBy marks every object a warm pool owns.
	LabelManagedBy = "managed-by"
	// AnnotationReservedAt marks claims whose final identity was bound before
	// the sidecar activation request. It lets a restarted controller distinguish
	// an abandoned reservation from a live legacy claim.
	AnnotationReservedAt = "warm.orchestrator.open-runtimes.io/reserved-at"

	ContainerShimInstall  = "shim-install"
	ContainerAgentInstall = "agent-install"
	ContainerProxy        = "proxy"
	ContainerWorkload     = "workload"

	defaultPoll      = 500 * time.Millisecond
	defaultColdWait  = 120 * time.Second // burst-cold: bound on a new pod turning warm-ready
	defaultServeWait = 60 * time.Second  // bound on a claimed pod answering
	defaultOrphan    = 60 * time.Second
	controlTick      = 2 * time.Second
	discardTimeout   = 10 * time.Second // bound on cleaning up a pod whose caller is gone
)

// Agent describes a binary to install into every warm pod's workspace, copied
// straight out of the image that publishes it — no vendoring, no download, and
// the version is pinned the way every other image is.
type Agent struct {
	Image string // image publishing the binary, e.g. ghcr.io/open-runtimes/sandbox:0.1.0
	// Source is the binary's path inside that image, Dest where it lands in the
	// workspace. Dest sits at the workspace root so the copy needs no mkdir, and
	// therefore no shell in the publishing image.
	Source string
	Dest   string
}

// Naming is the consumer's label and name contract. Each consumer keeps its
// own keys so its pods are its own — a rollout must never leave the other
// consumer's pods invisible (and therefore unreapable).
type Naming struct {
	ManagedBy  string // LabelManagedBy value, e.g. "deployments-service"
	Kind       string // metric attribute and log noun: "pool" | "sandbox"
	Pool       string // label key carrying the pool id, e.g. "pool.id"
	Claim      string // label key carrying the claim id
	Spec       string // annotation key carrying the claimed spec JSON
	NamePrefix string // warm pod name prefix, e.g. "pool"
	SecretName string // claim-key Secret name (tokens are derived from it)
}

// Config configures a Manager. The image, hardening, and placement knobs are
// the same ones the workload backends take.
type Config struct {
	Namespace              string
	SidecarImage           string // workload-sidecar (proxy) image
	ShimImage              string // pool-shim image for the shim-install init container
	SidecarImagePullPolicy string
	WorkerImagePullPolicy  string
	RunAsUser              int64
	Overcommit             kube.Overcommit
	Tolerations            []corev1.Toleration
	NodeSelector           map[string]string
	RuntimeClasses         map[string]string // isolation tier → RuntimeClass (internal/kube)
	OrphanTTL              time.Duration     // discard claimed-but-unlabeled pods (crashed mid-claim) after this
	Naming                 Naming

	// ReapUnpooled extends the control loop's end-of-life rule to claims whose
	// pool is not configured — workloads that brought their own pool of one.
	// Consumers that only ever claim from declared pools leave it off, so
	// removing a pool from the config cannot start reaping its live claims.
	ReapUnpooled bool

	// LeaderElection gates the control loop (replenishment + GC) to one replica.
	LeaderElection kube.LeaderElectionConfig

	// Metrics receives pool capacity and claim telemetry; may be nil.
	Metrics *observability.Metrics

	// Agent, when set, adds an init container that copies a contract-serving
	// binary out of a published image into the workspace — so a pool's image
	// serves the contract by running it, whatever the image is. Unset for
	// deployment pools, which late-bind their own command.
	Agent Agent
	// WorkloadEnv contributes environment to the workload container, on top of
	// the pool's own — the agent's SANDBOX_* settings, for consumers that
	// install it.
	WorkloadEnv func(p *pool.Spec) map[string]string

	// Client is the sidecar-facing surface. Nil uses the real HTTP one; unit
	// tests inject a fake, since fake-clientset pods have no reachable IPs.
	Client Client

	// Poll, ColdWait and ServeWait are shrunk by unit tests.
	Poll     time.Duration
	ColdWait time.Duration
	// ServeWait bounds the wait for a claimed pod to answer — artifact
	// materialization plus image startup.
	ServeWait time.Duration
}

// Manager owns one consumer's warm pools.
type Manager struct {
	client kubernetes.Interface
	cfg    Config
	pools  []pool.Pool
	byID   map[string]*pool.Pool
	sc     Client

	// stop halts the control loop Run launched.
	stop context.CancelFunc

	// installKey is the HMAC key claim tokens derive from (token.go),
	// get-or-created as the claim-key Secret and cached here.
	keyMu      sync.Mutex
	installKey []byte
	confirmed  sync.Map // pod name → sidecar accepted the metadata reservation
}

// Binding is the final workload identity atomically stamped while reserving a
// warm pod. Spec must already have secret material removed.
type Binding struct {
	Spec   any
	Labels map[string]string
	Owners []metav1.OwnerReference
}

// New creates a Manager over the configured pools.
func New(client kubernetes.Interface, pools []pool.Pool, cfg Config) *Manager {
	if cfg.Poll <= 0 {
		cfg.Poll = defaultPoll
	}
	if cfg.ColdWait <= 0 {
		cfg.ColdWait = defaultColdWait
	}
	if cfg.ServeWait <= 0 {
		cfg.ServeWait = defaultServeWait
	}
	if cfg.OrphanTTL <= 0 {
		cfg.OrphanTTL = defaultOrphan
	}
	if cfg.RuntimeClasses == nil {
		cfg.RuntimeClasses, _ = kube.ParseRuntimeClasses("")
	}
	sc := cfg.Client
	if sc == nil {
		sc = newHTTPClient()
	}
	return &Manager{client: client, cfg: cfg, pools: pools, byID: pool.ByID(pools), sc: sc}
}

// Pool returns one pool declaration, or nil.
func (m *Manager) Pool(id string) *pool.Pool { return m.byID[id] }

// Verify checks every pool's RuntimeClass and materializes the claim key,
// failing loudly rather than stranding warm pods Pending or unclaimable.
func (m *Manager) Verify(ctx context.Context) error {
	for i := range m.pools {
		f := &m.pools[i]
		rc := kube.RuntimeClassFor(m.cfg.RuntimeClasses, f.RuntimeClass)
		if rc == "" {
			continue
		}
		_, err := m.client.NodeV1().RuntimeClasses().Get(ctx, rc, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pool %q: RuntimeClass %q (tier %q) is not installed", f.ID, rc, f.RuntimeClass)
		}
		if err != nil {
			return apperrors.Internal("kubernetes.getRuntimeClass", err)
		}
	}
	_, err := m.claimKey(ctx)
	return err
}

// Pods lists one pool's managed pods.
func (m *Manager) Pods(ctx context.Context, poolID string) ([]corev1.Pod, error) {
	return m.list(ctx, m.selector(poolID))
}

// Claimed lists the claimed pods this consumer owns. Both filters are
// optional: an empty poolID matches any pool — for consumers whose claim ids
// are unique across pools, so their API paths need no pool — and an empty
// claimID matches every claim. One query therefore serves the status, list,
// and teardown paths.
func (m *Manager) Claimed(ctx context.Context, poolID, claimID string) ([]corev1.Pod, error) {
	selector := m.managed()
	if poolID != "" {
		selector += "," + m.cfg.Naming.Pool + "=" + poolID
	}
	if claimID != "" {
		// An id implies the label exists, so no separate existence term.
		return m.list(ctx, selector+","+m.cfg.Naming.Claim+"="+claimID)
	}
	return m.list(ctx, selector+","+m.cfg.Naming.Claim)
}

// PoolStatuses reports the configured pools with live warm/claimed counts.
func (m *Manager) PoolStatuses(ctx context.Context) ([]pool.Status, error) {
	statuses := make([]pool.Status, 0, len(m.pools))
	for i := range m.pools {
		p := &m.pools[i]
		pods, err := m.Pods(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		w, c := m.counts(pods)
		statuses = append(statuses, pool.Status{ID: p.ID, Image: p.Image, Size: p.Size, Warm: w, Claimed: c})
	}
	return statuses, nil
}

func (m *Manager) list(ctx context.Context, selector string) ([]corev1.Pod, error) {
	list, err := m.client.CoreV1().Pods(m.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listPods", err)
	}
	return list.Items, nil
}

func (m *Manager) selector(poolID string) string {
	return m.managed() + "," + m.cfg.Naming.Pool + "=" + poolID
}

func (m *Manager) managed() string {
	return LabelManagedBy + "=" + m.cfg.Naming.ManagedBy
}

// PoolID reads the pool a pod belongs to.
func (m *Manager) PoolID(pod *corev1.Pod) string { return pod.Labels[m.cfg.Naming.Pool] }

// ClaimID reads the claim a pod is bound to ("" while warm).
func (m *Manager) ClaimID(pod *corev1.Pod) string { return pod.Labels[m.cfg.Naming.Claim] }

// Spec decodes the claimed spec stored on a pod into v. A pod written by
// another release may be missing fields; that is not an error here.
func (m *Manager) Spec(pod *corev1.Pod, v any) {
	_ = json.Unmarshal([]byte(pod.Annotations[m.cfg.Naming.Spec]), v)
}

// counts splits a pool's pods into claimed and warm-READY (the claimable
// set); pods still starting are neither.
func (m *Manager) counts(pods []corev1.Pod) (warm, claimed int) {
	for i := range pods {
		switch {
		case m.ClaimID(&pods[i]) != "":
			claimed++
		case m.Claimable(&pods[i]):
			warm++
		}
	}
	return warm, claimed
}

// Claim wins one warm pod for the request and returns it. With no free pod the
// pool's burst policy decides: reject (429-mapped) or cold-create. A
// *claim.Poison error means the winning pod accepted the claim but its
// artifacts failed — the claim has failed, and the pod is discarded, never
// resold.
func (m *Manager) Claim(ctx context.Context, f *pool.Pool, req *workload.ClaimRequest, binding Binding) (*corev1.Pod, error) {
	key, err := m.claimKey(ctx)
	if err != nil {
		return nil, err
	}
	inv := &inventory{
		m: m, f: f, key: key, req: req, binding: binding,
		byName: make(map[string]*corev1.Pod),
	}
	unit, err := claim.Claim(ctx, inv, poster{m.sc}, m.recorder(), f.ID, f.Burst, req)
	if err != nil {
		return nil, err
	}
	pod := inv.byName[unit.ID]
	m.confirmed.Store(pod.Name, true)
	return pod, nil
}

// CreateClaimed creates a pod for one request and claims it: the poolless path,
// where a workload named an image instead of a pool. There is no warm pass and
// no burst policy — nothing was standing in this shape, and the pod is labeled
// with a poolID unique to the request, so it can never be offered to another
// claim. A pod that fails the claim is deleted here: this path created it, so
// nobody else is watching it.
func (m *Manager) CreateClaimed(ctx context.Context, s *pool.Spec, poolID string, req *workload.ClaimRequest, binding Binding) (_ *corev1.Pod, err error) {
	key, err := m.claimKey(ctx)
	if err != nil {
		return nil, err
	}
	pod, err := m.createClaimable(ctx, s, poolID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			m.Discard(ctx, pod.Name)
		}
	}()
	bound, err := m.reserve(ctx, pod, req.ClaimID, binding)
	if err != nil {
		return nil, err
	}
	if err = m.sc.Claim(ctx, pod.Status.PodIP, deriveClaimToken(key, pod.Name), req); err != nil {
		return nil, claim.Outcome(err, pod.Name)
	}
	m.confirmed.Store(bound.Name, true)
	return bound, nil
}

// Discard removes a pod that was created for one request and never handed over.
// It deletes on a context detached from the caller's, because the caller's is
// exactly what tends to be cancelled here — a client that hangs up mid-create
// cancels the request context, and a delete issued on it would fail too.
//
// This matters more than it looks: until a pod carries a claim label it is
// invisible to every control loop. No configured pool's reconcile selects a
// request-keyed pool id, and the unpooled reaper lists only pods that carry a
// claim. An undiscarded pod would hold its CPU and memory until the unclaimed
// sweep catches it a couple of minutes later, or forever on a build without one.
func (m *Manager) Discard(ctx context.Context, name string) {
	if err := m.DiscardErr(ctx, name); err != nil {
		slog.Error("Failed to discard a pod nothing owns; it will hold capacity until the unclaimed sweep",
			"pod", name, "error", err)
	}
}

// DiscardErr is Discard for callers that must know whether the pod really went.
// A caller about to report a failure needs this: saying "failed" of a workload
// whose pod is still running is worse than returning an error, because a claimed
// pod is invisible to the sweeps — they skip claimed pods, since a claimed pod is
// normally a live workload somebody wants.
func (m *Manager) DiscardErr(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
	defer cancel()
	return m.Delete(ctx, name)
}

// createClaimable creates a pod and waits (bounded) for it to turn warm-ready.
// A pod that never warms is deleted, so a cold start cannot leak capacity.
func (m *Manager) createClaimable(ctx context.Context, s *pool.Spec, poolID string) (_ *corev1.Pod, err error) {
	created, err := m.Create(ctx, s, poolID)
	if err != nil {
		return nil, err
	}
	// Every failure from here on — a lost API server, a cancelled request, a pod
	// that never warms — leaves a pod nobody asked for. One cleanup covers them
	// all, including the ones added later.
	defer func() {
		if err != nil {
			m.Discard(ctx, created.Name)
		}
	}()
	deadline := time.Now().Add(m.cfg.ColdWait)
	for {
		pod, getErr := m.client.CoreV1().Pods(m.cfg.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if getErr != nil {
			return nil, apperrors.Internal("kubernetes.getPod", getErr)
		}
		if m.Claimable(pod) {
			return pod, nil
		}
		if time.Now().After(deadline) {
			return nil, apperrors.Internal("kubernetes.coldClaim",
				fmt.Errorf("cold pod %s not warm-ready within %s", created.Name, m.cfg.ColdWait))
		}
		if sleepErr := m.sleep(ctx); sleepErr != nil {
			return nil, sleepErr
		}
	}
}

// reserve atomically stamps the claim's final identity before the sidecar may
// start the workload. resourceVersion turns the metadata patch into the claim
// serialization point: only one contender can reserve the pod it listed.
func (m *Manager) reserve(ctx context.Context, pod *corev1.Pod, claimID string, binding Binding) (*corev1.Pod, error) {
	encoded, err := json.Marshal(binding.Spec)
	if err != nil {
		return nil, apperrors.Internal("kubernetes.marshalSpec", err)
	}
	candidate := pod
	var bound *corev1.Pod
	lost := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		boundLabels := map[string]string{m.cfg.Naming.Claim: claimID}
		maps.Copy(boundLabels, binding.Labels)
		metadata := map[string]any{
			"resourceVersion": candidate.ResourceVersion,
			"labels":          boundLabels,
			"annotations": map[string]string{
				m.cfg.Naming.Spec:    string(encoded),
				AnnotationReservedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		}
		if binding.Owners != nil {
			metadata["ownerReferences"] = binding.Owners
		}
		patch, marshalErr := json.Marshal(map[string]any{"metadata": metadata})
		if marshalErr != nil {
			return marshalErr
		}
		bound, err = m.client.CoreV1().Pods(m.cfg.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
		if !apierrors.IsConflict(err) {
			return err
		}
		latest, getErr := m.client.CoreV1().Pods(m.cfg.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if !m.Claimable(latest) {
			lost = true
			return nil
		}
		candidate = latest
		return err
	})
	if lost {
		return nil, claim.ErrConflict
	}
	if err != nil {
		return nil, apperrors.Internal("kubernetes.reservePod", err)
	}
	return bound, nil
}

// Create creates one warm pod in the given shape, labeled for poolID. The name
// is chosen client-side (not GenerateName) so the claim token can be derived
// from it before creation.
func (m *Manager) Create(ctx context.Context, s *pool.Spec, poolID string) (*corev1.Pod, error) {
	key, err := m.claimKey(ctx)
	if err != nil {
		return nil, err
	}
	suffix, err := RandHex(5)
	if err != nil {
		return nil, apperrors.Internal("kubernetes.podName", err)
	}
	name := m.cfg.Naming.NamePrefix + "-" + poolID + "-" + suffix
	created, err := m.client.CoreV1().Pods(m.cfg.Namespace).Create(ctx, m.buildPod(s, poolID, name, deriveClaimToken(key, name)), metav1.CreateOptions{})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.createPod", err)
	}
	return created, nil
}

// Delete removes a pod, tolerating already-gone.
func (m *Manager) Delete(ctx context.Context, name string) error {
	err := m.client.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deletePod", err)
	}
	m.confirmed.Delete(name)
	return nil
}

// reservationAccepted answers from the request path's memory in the common
// case. After a controller restart it asks the sidecar once, then caches the
// confirmation for the pod's lifetime.
func (m *Manager) reservationAccepted(ctx context.Context, pod *corev1.Pod) bool {
	if pod.Annotations[AnnotationReservedAt] == "" {
		return true
	}
	if _, ok := m.confirmed.Load(pod.Name); ok {
		return true
	}
	state, err := m.sc.State(ctx, pod.Status.PodIP)
	if err != nil || !state.Claimed || state.ClaimID != m.ClaimID(pod) {
		return false
	}
	m.confirmed.Store(pod.Name, true)
	return true
}

// Ready checks that the K8s API server is reachable.
func (m *Manager) Ready(context.Context) error {
	_, err := m.client.Discovery().ServerVersion()
	return err
}

// Claimable reports whether a pod is in the free warm set: unclaimed, not
// being deleted, and warm-ready (the kubelet-probed sidecar /ready gate,
// surfaced as the pod Ready condition).
func (m *Manager) Claimable(pod *corev1.Pod) bool {
	return m.ClaimID(pod) == "" &&
		pod.DeletionTimestamp == nil &&
		pod.Status.PodIP != "" &&
		kube.PodReady(pod)
}

// sleep waits one poll interval, aborting with the context.
func (m *Manager) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.cfg.Poll):
		return nil
	}
}

// WorkloadTerminated returns the workload container's terminated state, or nil
// while it runs (or before it starts).
func WorkloadTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == ContainerWorkload {
			return pod.Status.ContainerStatuses[i].State.Terminated
		}
	}
	return nil
}

// inventory is the Kubernetes warm-unit surface behind the claim flow's seam:
// free units are claimable pool pods, a cold create pays the burst cold
// start. Pods are cached by name so the winner's object is at hand without
// re-fetching.
type inventory struct {
	m       *Manager
	f       *pool.Pool
	key     []byte
	req     *workload.ClaimRequest
	binding Binding
	byName  map[string]*corev1.Pod
}

func (inv *inventory) Free(ctx context.Context) ([]claim.Unit, error) {
	pods, err := inv.m.Pods(ctx, inv.f.ID)
	if err != nil {
		return nil, err
	}
	var units []claim.Unit
	for i := range pods {
		pod := &pods[i]
		if !inv.m.Claimable(pod) {
			continue
		}
		inv.byName[pod.Name] = pod
		units = append(units, inv.unitFor(pod))
	}
	return units, nil
}

func (inv *inventory) Reserve(ctx context.Context, unit claim.Unit) error {
	pod := inv.byName[unit.ID]
	if pod == nil {
		return apperrors.Internal("kubernetes.reservePod", fmt.Errorf("pod %s was not listed", unit.ID))
	}
	started := time.Now()
	bound, err := inv.m.reserve(ctx, pod, inv.req.ClaimID, inv.binding)
	if inv.m.cfg.Metrics != nil {
		inv.m.cfg.Metrics.RecordPoolReservation(ctx, inv.m.cfg.Naming.Kind, inv.f.ID, err == nil, time.Since(started).Seconds())
	}
	if err != nil {
		return err
	}
	inv.byName[unit.ID] = bound
	return nil
}

func (inv *inventory) Discard(ctx context.Context, unit claim.Unit) error {
	return inv.m.DiscardErr(ctx, unit.ID)
}

// Create pays the burst cold start: a pod in the pool's shape, waited into
// claimable.
func (inv *inventory) Create(ctx context.Context) (*claim.Unit, error) {
	pod, err := inv.m.createClaimable(ctx, &inv.f.Spec, inv.f.ID)
	if err != nil {
		return nil, err
	}
	inv.byName[pod.Name] = pod
	unit := inv.unitFor(pod)
	return &unit, nil
}

func (inv *inventory) unitFor(pod *corev1.Pod) claim.Unit {
	return claim.Unit{
		ID:    pod.Name,
		Addr:  pod.Status.PodIP,
		Token: deriveClaimToken(inv.key, pod.Name),
	}
}

// poster adapts the sidecar Client to the claim flow's Poster seam, so unit
// tests faking the Client intercept flow claims too.
type poster struct {
	sc Client
}

func (p poster) Post(ctx context.Context, u claim.Unit, req *workload.ClaimRequest) error {
	return p.sc.Claim(ctx, u.Addr, u.Token, req)
}

// recorder binds this consumer's kind onto the claim protocol's metrics,
// without producing a typed-nil interface when metrics are absent.
func (m *Manager) recorder() claim.Recorder {
	if m.cfg.Metrics == nil {
		return nil
	}
	return recorder{m: m.cfg.Metrics, kind: m.cfg.Naming.Kind}
}

type recorder struct {
	m    *observability.Metrics
	kind string
}

func (r recorder) RecordPoolConflict(ctx context.Context, id string) {
	r.m.RecordPoolConflict(ctx, r.kind, id)
}

func (r recorder) RecordPoolPoisoned(ctx context.Context, id string) {
	r.m.RecordPoolPoisoned(ctx, r.kind, id)
}

func (r recorder) RecordPoolBurst(ctx context.Context, id, policy string) {
	r.m.RecordPoolBurst(ctx, r.kind, id, policy)
}
