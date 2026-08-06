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
// leader-elected control loop. Deployment-pool activations and sandboxes both
// sit on it and differ only in how the pod is addressed once they hold it — an
// activation publishes a Service and route, a sandbox is reached through the
// wildcard edge by its capability token — and in the vocabulary they publish
// its phases in.
package warm

import (
	"context"
	"encoding/json"
	"fmt"
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
)

const (
	// LabelManagedBy marks every object a warm pool owns.
	LabelManagedBy = "managed-by"

	ContainerShimInstall  = "shim-install"
	ContainerAgentInstall = "agent-install"
	ContainerProxy        = "proxy"
	ContainerWorkload     = "workload"

	defaultPoll      = 500 * time.Millisecond
	defaultColdWait  = 120 * time.Second // burst-cold: bound on a new pod turning warm-ready
	defaultServeWait = 60 * time.Second  // bound on a claimed pod answering
	defaultOrphan    = 60 * time.Second
	controlTick      = 2 * time.Second
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
	Claim      string // label key carrying the claim id, e.g. "pool.activation"
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
	WorkloadEnv func(p *pool.Pool) map[string]string

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
func (m *Manager) Claim(ctx context.Context, f *pool.Pool, req *workload.ClaimRequest) (*corev1.Pod, error) {
	key, err := m.claimKey(ctx)
	if err != nil {
		return nil, err
	}
	inv := &inventory{m: m, f: f, key: key, byName: make(map[string]*corev1.Pod)}
	unit, err := claim.Claim(ctx, inv, poster{m.sc}, m.recorder(), f.ID, f.Burst, req)
	if err != nil {
		return nil, err
	}
	return inv.byName[unit.ID], nil
}

// Bind stamps the accepted claim onto the pod: the claim label (the status,
// list, and GC key), the spec annotation (status reconstruction), and any
// consumer labels the claim routes by. Callers must strip secret material from
// spec first — the pod object is not a safe place to rest it.
func (m *Manager) Bind(ctx context.Context, podName, claimID string, spec any, labels map[string]string) error {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return apperrors.Internal("kubernetes.marshalSpec", err)
	}
	bound := map[string]string{m.cfg.Naming.Claim: claimID}
	maps.Copy(bound, labels)
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":      bound,
		"annotations": map[string]string{m.cfg.Naming.Spec: string(encoded)},
	}})
	if err != nil {
		return apperrors.Internal("kubernetes.marshalPatch", err)
	}
	// A crash before this patch leaves a claimed-but-unlabeled pod; orphan GC
	// discards it after OrphanTTL — orphans are garbage, never resold.
	if _, err := m.client.CoreV1().Pods(m.cfg.Namespace).Patch(ctx, podName, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return apperrors.Internal("kubernetes.bindPod", err)
	}
	return nil
}

// Create creates one warm pod. The name is chosen client-side (not
// GenerateName) so the claim token can be derived from it before creation.
func (m *Manager) Create(ctx context.Context, f *pool.Pool) (*corev1.Pod, error) {
	key, err := m.claimKey(ctx)
	if err != nil {
		return nil, err
	}
	suffix, err := RandHex(5)
	if err != nil {
		return nil, apperrors.Internal("kubernetes.podName", err)
	}
	name := m.cfg.Naming.NamePrefix + "-" + f.ID + "-" + suffix
	created, err := m.client.CoreV1().Pods(m.cfg.Namespace).Create(ctx, m.buildPod(f, name, deriveClaimToken(key, name)), metav1.CreateOptions{})
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
	return nil
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
	m      *Manager
	f      *pool.Pool
	key    []byte
	byName map[string]*corev1.Pod
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

// Create creates a pod and waits for it to turn warm-ready (bounded). A pod
// that never warms is deleted so the burst does not leak capacity beyond the
// pool size.
func (inv *inventory) Create(ctx context.Context) (*claim.Unit, error) {
	created, err := inv.m.Create(ctx, inv.f)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(inv.m.cfg.ColdWait)
	for {
		pod, err := inv.m.client.CoreV1().Pods(inv.m.cfg.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return nil, apperrors.Internal("kubernetes.getPod", err)
		}
		if inv.m.Claimable(pod) {
			inv.byName[pod.Name] = pod
			unit := inv.unitFor(pod)
			return &unit, nil
		}
		if time.Now().After(deadline) {
			_ = inv.m.Delete(ctx, created.Name)
			return nil, apperrors.Internal("kubernetes.coldClaim",
				fmt.Errorf("cold pod %s not warm-ready within %s", created.Name, inv.m.cfg.ColdWait))
		}
		if err := inv.m.sleep(ctx); err != nil {
			return nil, err
		}
	}
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
