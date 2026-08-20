package warm

import (
	"context"
	"errors"
	"orchestrator/internal/claim"
	"orchestrator/internal/pool"
	"orchestrator/internal/workload"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNS = "orchestrator"

// testNaming is a consumer's label contract, standing in for the pool one.
var testNaming = Naming{
	ManagedBy:  "deployments-service",
	Kind:       "pool",
	Pool:       "pool.id",
	Claim:      "pool.activation",
	Spec:       "pool.activation-spec",
	NamePrefix: "pool",
	SecretName: "pool-claim-key",
}

// fakeSidecar fakes the sidecar surface per pod IP: fake-clientset pods have no
// reachable sidecars, and the pod IP is the claim protocol's address.
type fakeSidecar struct {
	mu       sync.Mutex
	conflict map[string]bool                // Claim → 409
	poison   map[string]bool                // Claim → 422 (artifacts failed)
	state    map[string]workload.ClaimState // State responses
	notReady map[string]bool                // Ready → false (default ready)
	requests map[string]int64               // Requests responses
	claimed  []string                       // successful claim IPs, in order
	tokens   []string                       // bearer tokens presented with them
	last     *workload.ClaimRequest
}

func newFakeSidecar() *fakeSidecar {
	return &fakeSidecar{
		conflict: map[string]bool{},
		poison:   map[string]bool{},
		state:    map[string]workload.ClaimState{},
		notReady: map[string]bool{},
		requests: map[string]int64{},
	}
}

func (f *fakeSidecar) Claim(_ context.Context, podIP, token string, req *workload.ClaimRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflict[podIP] {
		return claim.ErrConflict
	}
	if f.poison[podIP] {
		return &claim.Poison{Msg: "artifacts failed: boom"}
	}
	f.claimed = append(f.claimed, podIP)
	f.tokens = append(f.tokens, token)
	f.last = req
	return nil
}

func (f *fakeSidecar) State(_ context.Context, podIP string) (*workload.ClaimState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.state[podIP]
	return &state, nil
}

func (f *fakeSidecar) Ready(_ context.Context, podIP string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.notReady[podIP]
}

func (f *fakeSidecar) Requests(_ context.Context, podIP string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[podIP], nil
}

func testPool(id string) pool.Pool {
	return pool.Pool{ID: id, Size: 1, Burst: pool.BurstReject, Spec: pool.Spec{Image: "runtime:latest", Port: 8080}}
}

func newTestManager(t *testing.T, pools ...pool.Pool) (*Manager, *fake.Clientset, *fakeSidecar) {
	t.Helper()
	cs := fake.NewClientset()
	sidecar := newFakeSidecar()
	m := New(cs, pools, Config{
		Namespace:    testNS,
		SidecarImage: "sidecar:latest",
		ShimImage:    "shim:latest",
		RunAsUser:    65532,
		Naming:       testNaming,
		Client:       sidecar,
		Poll:         time.Millisecond,
		ColdWait:     time.Second,
		ServeWait:    50 * time.Millisecond,
	})
	// Normally get-or-created from the claim-key Secret on Start.
	m.installKey = testInstallKey
	return m, cs, sidecar
}

// warmPodFixture is a running, warm-ready pool pod as the replenisher would
// have produced it (labels, Ready condition, IP — no token anywhere: claim
// tokens are derived from the pod name).
func warmPodFixture(m *Manager, poolID, name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNS,
			Labels:      m.PoolLabels(poolID),
			Annotations: map[string]string{},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  ContainerWorkload,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

// claimedPodFixture is a bound pod: claim label + spec annotation.
func claimedPodFixture(m *Manager, poolID, name, ip, claimID, spec string) *corev1.Pod {
	pod := warmPodFixture(m, poolID, name, ip)
	pod.Labels[m.cfg.Naming.Claim] = claimID
	pod.Annotations[m.cfg.Naming.Spec] = spec
	return pod
}

func addPod(t *testing.T, cs *fake.Clientset, pod *corev1.Pod) {
	t.Helper()
	if _, err := cs.CoreV1().Pods(testNS).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod %s: %v", pod.Name, err)
	}
}

func podGone(t *testing.T, cs *fake.Clientset, name string) bool {
	t.Helper()
	_, err := cs.CoreV1().Pods(testNS).Get(t.Context(), name, metav1.GetOptions{})
	return apierrors.IsNotFound(err)
}

func TestClaim_ConflictRetriesNextPod(t *testing.T) {
	t.Parallel()
	m, cs, sidecar := newTestManager(t, testPool("std"))
	addPod(t, cs, warmPodFixture(m, "std", "pod-a", "10.0.0.1"))
	addPod(t, cs, warmPodFixture(m, "std", "pod-b", "10.0.0.2"))
	sidecar.conflict["10.0.0.1"] = true // a racing replica won pod-a

	pod, err := m.Claim(t.Context(), m.Pool("std"), &workload.ClaimRequest{ActivationID: "act1", Command: "serve"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if pod.Name != "pod-b" {
		t.Errorf("want the next warm pod after a 409, got %s", pod.Name)
	}
}

func TestClaim_TokenIsDerivedFromPodName(t *testing.T) {
	t.Parallel()
	m, cs, sidecar := newTestManager(t, testPool("std"))
	addPod(t, cs, warmPodFixture(m, "std", "pod-a", "10.0.0.1"))

	if _, err := m.Claim(t.Context(), m.Pool("std"), &workload.ClaimRequest{ActivationID: "act1"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(sidecar.tokens) != 1 || sidecar.tokens[0] != deriveClaimToken(m.installKey, "pod-a") {
		t.Errorf("claim must present the token derived from the pod name, got %v", sidecar.tokens)
	}
}

func TestClaim_ExhaustedRejects(t *testing.T) {
	t.Parallel()
	m, _, _ := newTestManager(t, testPool("std")) // burst: reject, and no warm pods

	if _, err := m.Claim(t.Context(), m.Pool("std"), &workload.ClaimRequest{ActivationID: "act1"}); err == nil {
		t.Fatal("want an exhausted-pool error")
	}
}

func TestBind_StampsClaimAndSpec(t *testing.T) {
	t.Parallel()
	m, cs, _ := newTestManager(t, testPool("std"))
	addPod(t, cs, warmPodFixture(m, "std", "pod-a", "10.0.0.1"))

	if err := m.Bind(t.Context(), "pod-a", "act1", map[string]string{"command": "serve"}, map[string]string{"sandbox.token": "abc"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNS).Get(t.Context(), "pod-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if m.ClaimID(pod) != "act1" {
		t.Errorf("claim label: got %v", pod.Labels)
	}
	var spec map[string]string
	m.Spec(pod, &spec)
	if spec["command"] != "serve" {
		t.Errorf("spec annotation: got %v", pod.Annotations)
	}
}

func TestCounts_WarmExcludesClaimedAndUnready(t *testing.T) {
	t.Parallel()
	m, _, _ := newTestManager(t, testPool("std"))
	warmReady := warmPodFixture(m, "std", "pod-a", "10.0.0.1")
	claimedPod := claimedPodFixture(m, "std", "pod-b", "10.0.0.2", "act1", "{}")
	starting := warmPodFixture(m, "std", "pod-c", "")
	starting.Status.Conditions = nil

	warm, claimed := m.counts([]corev1.Pod{*warmReady, *claimedPod, *starting})
	if warm != 1 || claimed != 1 {
		t.Errorf("want 1 warm + 1 claimed (a starting pod is neither), got %d/%d", warm, claimed)
	}
}

// A client that hangs up mid-create cancels the request context, and the pod
// created for that request is already running. Nothing else will ever collect it
// — its pool id is the request's, so no reconcile selects it, and it carries no
// claim label for the unpooled reaper to find — so the create path has to clean
// up after itself, on a context its caller cannot cancel.
func TestCreateClaimed_DiscardsItsPodWhenTheCallerGoesAway(t *testing.T) {
	t.Parallel()
	m, cs, _ := newTestManager(t)
	ctx, cancel := context.WithCancel(t.Context())

	// The pod never turns claimable, so the create is still polling when the
	// caller goes away.
	cancelOnPodCreate(cs, cancel)

	if _, err := m.CreateClaimed(ctx, &pool.Spec{Image: "img", Port: 3000}, "solo",
		&workload.ClaimRequest{ActivationID: "solo", Command: "run"}); err == nil {
		t.Fatal("a cancelled create must fail")
	}

	pods, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("the abandoned pod must be discarded, got %d still running", len(pods.Items))
	}
}

// cancelOnPodCreate fires cancel as soon as the code under test creates a pod:
// the client hanging up at the worst possible moment, with the pod live and
// nothing yet recorded about it.
func cancelOnPodCreate(cs *fake.Clientset, cancel context.CancelFunc) {
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})
}

// The serving-wait timeout deletes the pod and hands its caller a reason to
// report as a failed workload. If that delete fails, the reason is a lie the
// caller cannot detect and nothing downstream can repair — the pod is claimed, so
// both sweeps skip it, and a caller told "failed" never deletes it. So a failed
// delete has to surface as an error instead.
func TestAwait_UnservingPodThatWillNotDeleteIsAnError(t *testing.T) {
	t.Parallel()
	m, cs, sidecar := newTestManager(t)
	pod := warmPodFixture(m, "std", "pool-std-aaaaa", "10.0.0.1")
	addPod(t, cs, pod)
	sidecar.notReady["10.0.0.1"] = true // never turns serving-ready
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})

	unserved, err := m.Await(t.Context(), pod)
	if err == nil {
		t.Error("a pod that could not be deleted must not be reported as a plain failure")
	}
	if unserved != "" {
		t.Errorf("no reason may be returned when the pod is still running, got %q", unserved)
	}
}
