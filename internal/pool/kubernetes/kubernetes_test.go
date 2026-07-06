package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"orchestrator/pkg/pool"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	krand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

const testNS = "orchestrator"

// testInstallKey is a fixed claim-token HMAC key for deterministic tests.
var testInstallKey = []byte("0123456789abcdef0123456789abcdef")

// fakeClaims fakes the sidecar surface per pod IP: fake-clientset pods have
// no reachable sidecars, and the pod IP is the claim protocol's address.
type fakeClaims struct {
	mu        sync.Mutex
	conflict  map[string]bool             // Claim → 409
	state     map[string]proxy.ClaimState // State responses
	notReady  map[string]bool             // Ready → false (default ready)
	requests  map[string]int64            // Requests responses
	claimed   []string                    // successful claim IPs, in order
	tokens    []string                    // bearer tokens presented with them
	lastClaim *proxy.ClaimRequest
}

func newFakeClaims() *fakeClaims {
	return &fakeClaims{
		conflict: map[string]bool{},
		state:    map[string]proxy.ClaimState{},
		notReady: map[string]bool{},
		requests: map[string]int64{},
	}
}

func (f *fakeClaims) Claim(_ context.Context, podIP, token string, req *proxy.ClaimRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflict[podIP] {
		return errClaimConflict
	}
	f.claimed = append(f.claimed, podIP)
	f.tokens = append(f.tokens, token)
	f.lastClaim = req
	return nil
}

func (f *fakeClaims) State(_ context.Context, podIP string) (*proxy.ClaimState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.state[podIP]
	return &state, nil
}

func (f *fakeClaims) Ready(_ context.Context, podIP string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.notReady[podIP]
}

func (f *fakeClaims) Requests(_ context.Context, podIP string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[podIP], nil
}

func newTestOrchestrator(t *testing.T, pools ...pool.Pool) (*Orchestrator, *fake.Clientset, *fakeClaims) {
	t.Helper()
	cs := fake.NewClientset()
	// The fake tracker does not resolve GenerateName; do it the way the API
	// server would so createWarmPod works.
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Name == "" && pod.GenerateName != "" {
			pod.Name = pod.GenerateName + krand.String(5)
		}
		return false, nil, nil
	})
	cfg := Config{
		SidecarImage:   "sidecar:latest",
		ShimImage:      "shim:latest",
		Namespace:      testNS,
		RunAsUser:      65532,
		GatewayEnabled: true,
		PoolDomain:     "pools.example.com",
		Pools:          pools,
	}
	cfg.applyDefaults()
	o := wireOrchestrator(cs, gatewayfake.NewClientset(), cfg)
	claims := newFakeClaims()
	o.claims = claims
	o.installKey = testInstallKey // normally get-or-created from the pool-claim-key Secret on Start
	o.poll = time.Millisecond
	o.coldWait = time.Second
	o.serveWait = 50 * time.Millisecond
	return o, cs, claims
}

// warmPodFixture is a running, warm-ready pool pod as the replenisher would
// have produced it (labels, Ready condition, IP — no token anywhere: claim
// tokens are derived from the pod name).
func warmPodFixture(poolID, name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNS,
			Labels:      poolLabels(poolID),
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

func addPod(t *testing.T, cs *fake.Clientset, pod *corev1.Pod) {
	t.Helper()
	if _, err := cs.CoreV1().Pods(testNS).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod %s: %v", pod.Name, err)
	}
}

// claimedPodFixture is a bound pod: activation label + spec annotation.
func claimedPodFixture(poolID, name, ip, activationID string, act pool.Activation) *corev1.Pod {
	act.ID = activationID
	spec, _ := json.Marshal(act)
	pod := warmPodFixture(poolID, name, ip)
	pod.Labels[LabelActivation] = activationID
	pod.Annotations[AnnotationActivationSpec] = string(spec)
	return pod
}

func setWorkloadTerminated(pod *corev1.Pod, exitCode int32, finishedAt time.Time) {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: ContainerWorkload,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   exitCode,
			FinishedAt: metav1.NewTime(finishedAt),
		}},
	}}
}

func getPod(t *testing.T, cs *fake.Clientset, name string) *corev1.Pod {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNS).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod
}

func podGone(t *testing.T, cs *fake.Clientset, name string) bool {
	t.Helper()
	_, err := cs.CoreV1().Pods(testNS).Get(t.Context(), name, metav1.GetOptions{})
	return apierrors.IsNotFound(err)
}

func execPool(id string) pool.Pool {
	return pool.Pool{ID: id, Image: "runtime:latest", Size: 1, Burst: pool.BurstReject}
}

func httpPool(id string) pool.Pool {
	p := execPool(id)
	p.Port = 8080
	return p
}

func TestActivate_ExecReturnsExitCodeAndOutput(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, execPool("std"))
	pod := warmPodFixture("std", "pod-a", "10.0.0.1")
	setWorkloadTerminated(pod, 7, time.Now())
	addPod(t, cs, pod)

	status, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "run"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.State != pool.StateExited || status.ExitCode == nil || *status.ExitCode != 7 {
		t.Errorf("want exited/7, got %s/%v", status.State, status.ExitCode)
	}
	if status.Output != "fake logs" { // the fake clientset's canned log body
		t.Errorf("Output: want 'fake logs', got %q", status.Output)
	}
	if status.PodID != "pod-a" || status.PoolID != "std" {
		t.Errorf("identity: got %+v", status)
	}

	// The claim carried the pod's DERIVED token and the exec shape (Port 0).
	if len(claims.claimed) != 1 || claims.claimed[0] != "10.0.0.1" || claims.tokens[0] != deriveClaimToken(testInstallKey, "pod-a") {
		t.Errorf("claim: got IPs %v tokens %v", claims.claimed, claims.tokens)
	}
	if claims.lastClaim.ActivationID != "act1" || claims.lastClaim.Command != "run" || claims.lastClaim.Port != 0 {
		t.Errorf("claim request: got %+v", claims.lastClaim)
	}

	// The pod is bound: activation label + reconstructable spec annotation.
	bound := getPod(t, cs, "pod-a")
	if bound.Labels[LabelActivation] != "act1" {
		t.Errorf("activation label: got %q", bound.Labels[LabelActivation])
	}
	var spec pool.Activation
	if err := json.Unmarshal([]byte(bound.Annotations[AnnotationActivationSpec]), &spec); err != nil || spec.Command != "run" {
		t.Errorf("spec annotation: got %q (err %v)", bound.Annotations[AnnotationActivationSpec], err)
	}
}

func TestActivate_ClaimConflictRetriesNextPod(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, execPool("std"))
	first := warmPodFixture("std", "pod-a", "10.0.0.1")
	setWorkloadTerminated(first, 0, time.Now())
	addPod(t, cs, first)
	second := warmPodFixture("std", "pod-b", "10.0.0.2")
	setWorkloadTerminated(second, 0, time.Now())
	addPod(t, cs, second)
	claims.conflict["10.0.0.1"] = true // a racing replica won pod-a

	status, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "run"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.PodID != "pod-b" {
		t.Errorf("want the 409 loser to claim pod-b, got %s", status.PodID)
	}
	if len(claims.claimed) != 1 || claims.claimed[0] != "10.0.0.2" {
		t.Errorf("claimed IPs: got %v", claims.claimed)
	}
}

func TestActivate_BurstRejectIsExhausted(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, execPool("std"))

	_, err := o.Activate(t.Context(), "std", &pool.Activation{Command: "run"})
	if !errors.Is(err, apperrors.ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	if got := apperrors.HTTPStatus(err); got != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus: want 429, got %d", got)
	}
}

func TestActivate_BurstColdCreatesPod(t *testing.T) {
	t.Parallel()
	p := execPool("std")
	p.Burst = pool.BurstCold
	o, cs, claims := newTestOrchestrator(t, p)
	// Make every created pod immediately warm-ready with a finished workload,
	// as the kubelet eventually would.
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.9.9",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		}
		setWorkloadTerminated(pod, 0, time.Now())
		return false, nil, nil
	})

	status, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "run"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.State != pool.StateExited {
		t.Errorf("want exited, got %s", status.State)
	}
	if len(claims.claimed) != 1 || claims.claimed[0] != "10.0.9.9" {
		t.Errorf("want the cold pod claimed, got %v", claims.claimed)
	}
}

func TestActivate_ExecTimeoutDiscardsPod(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, execPool("std"))
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1")) // workload never exits

	status, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "sleep", TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.State != pool.StateFailed || status.Error != "timeout" {
		t.Errorf("want failed/timeout, got %s/%q", status.State, status.Error)
	}
	if !podGone(t, cs, "pod-a") {
		t.Error("want the timed-out pod deleted")
	}
}

func TestActivate_HTTPCreatesServiceAndRoute(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))

	status, err := o.Activate(t.Context(), "web", &pool.Activation{ID: "site", Command: "serve"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.State != pool.StateReady || status.URL != "http://site.pools.example.com" {
		t.Errorf("want ready at http://site.pools.example.com, got %s %q", status.State, status.URL)
	}
	if claims.lastClaim.Port != 8080 {
		t.Errorf("claim Port: want 8080, got %d", claims.lastClaim.Port)
	}

	svc, err := cs.CoreV1().Services(testNS).Get(t.Context(), "act-site", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Selector[LabelActivation] != "site" {
		t.Errorf("service selector: got %v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != int(proxy.DefaultProxyPort) {
		t.Errorf("service ports: got %+v", svc.Spec.Ports)
	}
	if svc.Labels[LabelPoolID] != "web" || svc.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("service labels: got %v", svc.Labels)
	}

	route, err := o.gateway.GatewayV1().HTTPRoutes(testNS).Get(t.Context(), "act-site", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != "site.pools.example.com" {
		t.Errorf("route hostnames: got %v", route.Spec.Hostnames)
	}
	parent := route.Spec.ParentRefs[0]
	if string(parent.Name) != "orchestrator" || string(*parent.Namespace) != testNS {
		t.Errorf("route parentRef: got %+v", parent)
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("route rules: got %+v", route.Spec.Rules)
	}
	backend := route.Spec.Rules[0].BackendRefs[0]
	if string(backend.Name) != "act-site" || *backend.Port != 80 {
		t.Errorf("route backendRef: got %+v", backend)
	}
}

func TestActivate_HTTPCustomHost(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))

	status, err := o.Activate(t.Context(), "web", &pool.Activation{ID: "site", Host: "my.example.com", Command: "serve"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.URL != "http://my.example.com" {
		t.Errorf("URL: got %q", status.URL)
	}
	route, err := o.gateway.GatewayV1().HTTPRoutes(testNS).Get(t.Context(), "act-site", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if string(route.Spec.Hostnames[0]) != "my.example.com" {
		t.Errorf("route hostname: got %v", route.Spec.Hostnames)
	}
}

func TestActivate_HTTPNeverServingFailsAndTearsDown(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))
	claims.notReady["10.0.0.1"] = true // never turns serving-ready after the claim

	status, err := o.Activate(t.Context(), "web", &pool.Activation{ID: "site", Command: "serve"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.State != pool.StateFailed {
		t.Errorf("want failed, got %s", status.State)
	}
	if !podGone(t, cs, "pod-a") {
		t.Error("want the never-ready pod discarded")
	}
	if _, err := cs.CoreV1().Services(testNS).Get(t.Context(), "act-site", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("want the service torn down, got %v", err)
	}
}

func TestActivate_DuplicateActivationConflicts(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, execPool("std"))
	addPod(t, cs, claimedPodFixture("std", "pod-a", "10.0.0.1", "act1", pool.Activation{Command: "run"}))

	_, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "run"})
	if !errors.Is(err, apperrors.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestActivate_UnknownPoolNotFound(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, execPool("std"))
	_, err := o.Activate(t.Context(), "nope", &pool.Activation{Command: "run"})
	if !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStatusFromPod_Derivation(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, execPool("std"), httpPool("web"))

	exec, web := o.pools["std"], o.pools["web"]
	running := claimedPodFixture("std", "pod-a", "10.0.0.1", "act1", pool.Activation{Command: "run"})

	exited := claimedPodFixture("std", "pod-b", "10.0.0.2", "act2", pool.Activation{Command: "run"})
	setWorkloadTerminated(exited, 3, time.Now())

	infraFailed := claimedPodFixture("std", "pod-c", "10.0.0.3", "act3", pool.Activation{Command: "run"})
	infraFailed.Status.Phase = corev1.PodFailed
	infraFailed.Status.ContainerStatuses = nil
	infraFailed.Status.Reason = "Evicted"

	deleting := claimedPodFixture("std", "pod-d", "10.0.0.4", "act4", pool.Activation{Command: "run"})
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	serving := claimedPodFixture("web", "pod-e", "10.0.0.5", "act5", pool.Activation{Command: "serve"})

	starting := claimedPodFixture("web", "pod-f", "10.0.0.6", "act6", pool.Activation{Command: "serve"})
	starting.Status.Conditions = nil // not yet serving-ready

	httpExited := claimedPodFixture("web", "pod-g", "10.0.0.7", "act7", pool.Activation{Command: "serve"})
	setWorkloadTerminated(httpExited, 1, time.Now())

	tests := []struct {
		name      string
		pool      *pool.Pool
		pod       *corev1.Pod
		wantState string
		wantCode  *int
	}{
		{"exec running is ready", exec, running, pool.StateReady, nil},
		{"exec terminated is exited with code", exec, exited, pool.StateExited, ptrInt(3)},
		{"infra failure is failed", exec, infraFailed, pool.StateFailed, nil},
		{"deletion in flight is deactivating", exec, deleting, pool.StateDeactivating, nil},
		{"http serving-ready is ready", web, serving, pool.StateReady, nil},
		{"http not yet ready is activating", web, starting, pool.StateActivating, nil},
		{"http workload exit is failed", web, httpExited, pool.StateFailed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := o.statusFromPod(tt.pool, tt.pod)
			if got.State != tt.wantState {
				t.Errorf("state: want %s, got %s (error %q)", tt.wantState, got.State, got.Error)
			}
			if tt.wantCode != nil && (got.ExitCode == nil || *got.ExitCode != *tt.wantCode) {
				t.Errorf("exit code: want %d, got %v", *tt.wantCode, got.ExitCode)
			}
			if tt.pool.HTTP() && got.URL == "" {
				t.Error("want an URL on HTTP activations")
			}
		})
	}
}

func ptrInt(v int) *int { return &v }

func TestStatus_ReconstructsFromAnnotation(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, claimedPodFixture("web", "pod-a", "10.0.0.1", "site", pool.Activation{Host: "my.example.com", Command: "serve"}))

	status, err := o.Status(t.Context(), "web", "site")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.URL != "http://my.example.com" || status.State != pool.StateReady {
		t.Errorf("got %+v", status)
	}
}

func TestStatus_NotFound(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, execPool("std"))
	if _, err := o.Status(t.Context(), "std", "ghost"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestList_ReturnsClaimedPodsOnly(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, execPool("std"))
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	addPod(t, cs, claimedPodFixture("std", "pod-b", "10.0.0.2", "act1", pool.Activation{Command: "run"}))
	addPod(t, cs, claimedPodFixture("std", "pod-c", "10.0.0.3", "act2", pool.Activation{Command: "run"}))

	list, err := o.List(t.Context(), "std")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 activations, got %d", len(list))
	}
}

func TestPools_Counts(t *testing.T) {
	t.Parallel()
	p := execPool("std")
	p.Size = 3
	o, cs, _ := newTestOrchestrator(t, p)
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	notReady := warmPodFixture("std", "pod-b", "10.0.0.2")
	notReady.Status.Conditions = nil // still starting: neither warm nor claimed
	addPod(t, cs, notReady)
	addPod(t, cs, claimedPodFixture("std", "pod-c", "10.0.0.3", "act1", pool.Activation{Command: "run"}))

	statuses, err := o.Pools(t.Context())
	if err != nil {
		t.Fatalf("Pools: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("want 1 pool, got %d", len(statuses))
	}
	s := statuses[0]
	if s.Warm != 1 || s.Claimed != 1 || s.Size != 3 {
		t.Errorf("want warm=1 claimed=1 size=3, got %+v", s)
	}
}

func TestDeactivate_TearsDownEverything(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))
	if _, err := o.Activate(t.Context(), "web", &pool.Activation{ID: "site", Command: "serve"}); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if err := o.Deactivate(t.Context(), "web", "site"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if !podGone(t, cs, "pod-a") {
		t.Error("want the pod deleted")
	}
	if _, err := cs.CoreV1().Services(testNS).Get(t.Context(), "act-site", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("want the service deleted, got %v", err)
	}
	if _, err := o.gateway.GatewayV1().HTTPRoutes(testNS).Get(t.Context(), "act-site", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("want the route deleted, got %v", err)
	}
}

// --- secret material at rest (docs/design/security.md) ---

func TestDeriveClaimToken_DeterministicPerPod(t *testing.T) {
	t.Parallel()
	a1 := deriveClaimToken(testInstallKey, "pool-std-aaaaa")
	a2 := deriveClaimToken(testInstallKey, "pool-std-aaaaa")
	b := deriveClaimToken(testInstallKey, "pool-std-bbbbb")
	other := deriveClaimToken([]byte("another-install-key-32-bytes-xx!"), "pool-std-aaaaa")
	if a1 != a2 {
		t.Error("token must be deterministic for (key, podName)")
	}
	if a1 == b {
		t.Error("tokens must differ across pods")
	}
	if a1 == other {
		t.Error("tokens must differ across install keys")
	}
	if len(a1) != 64 { // hex(HMAC-SHA256)
		t.Errorf("token length: want 64 hex chars, got %d", len(a1))
	}
}

func TestClaimKey_GetOrCreateIdempotent(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, execPool("std"))
	o.installKey = nil // exercise the get-or-create path

	first, err := o.claimKey(t.Context())
	if err != nil {
		t.Fatalf("claimKey: %v", err)
	}
	if len(first) != claimKeyBytes {
		t.Fatalf("key length: want %d, got %d", claimKeyBytes, len(first))
	}
	secret, err := cs.CoreV1().Secrets(testNS).Get(t.Context(), claimKeySecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected the pool-claim-key Secret: %v", err)
	}
	if string(secret.Data[claimKeySecretKey]) != string(first) {
		t.Error("cached key must match the stored Secret")
	}

	// A second orchestrator against the same cluster adopts the same key.
	o2 := wireOrchestrator(cs, o.gateway, o.cfg)
	second, err := o2.claimKey(t.Context())
	if err != nil {
		t.Fatalf("claimKey (second): %v", err)
	}
	if string(second) != string(first) {
		t.Error("get-or-create must be idempotent across replicas")
	}
}

func TestCreateWarmPod_InjectsDerivedTokenNoAnnotation(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, execPool("std"))

	created, err := o.createWarmPod(t.Context(), o.pools["std"])
	if err != nil {
		t.Fatalf("createWarmPod: %v", err)
	}
	pod := getPod(t, cs, created.Name)
	if _, ok := pod.Annotations["pool.claim-token"]; ok {
		t.Error("claim token must never be annotated on the pod")
	}
	want := deriveClaimToken(testInstallKey, pod.Name)
	found := false
	for _, c := range pod.Spec.InitContainers {
		if c.Name != ContainerProxy {
			continue
		}
		for _, env := range c.Env {
			if env.Name == proxy.EnvClaimToken {
				found = true
				if env.Value != want {
					t.Errorf("POOL_CLAIM_TOKEN: want the derived token, got %q", env.Value)
				}
			}
		}
	}
	if !found {
		t.Error("sidecar must still receive POOL_CLAIM_TOKEN env")
	}
}

func TestBindPod_StripsCallbackKeyFromAnnotation(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))

	act := &pool.Activation{
		ID:       "site",
		Command:  "serve",
		Callback: &deployment.Callback{URL: "http://callbacks.test/hook", Key: "super-secret"},
	}
	if _, err := o.Activate(t.Context(), "web", act); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	raw := getPod(t, cs, "pod-a").Annotations[AnnotationActivationSpec]
	if strings.Contains(raw, "super-secret") {
		t.Errorf("annotation must not carry the callback key: %s", raw)
	}
	var stored pool.Activation
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("unmarshal annotation: %v", err)
	}
	if stored.Callback == nil || stored.Callback.URL != "http://callbacks.test/hook" {
		t.Errorf("callback URL must survive redaction: %+v", stored.Callback)
	}
	if stored.Callback.Key != "" {
		t.Errorf("callback key must be stripped, got %q", stored.Callback.Key)
	}
	// The in-flight request keeps the full callback for delivery.
	if act.Callback.Key != "super-secret" {
		t.Error("redaction must not mutate the caller's activation")
	}
}

func TestDeactivate_NotFound(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, execPool("std"))
	if err := o.Deactivate(t.Context(), "std", "ghost"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
