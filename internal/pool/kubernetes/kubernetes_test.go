package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/deployment"
	"orchestrator/internal/pool"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	conflict  map[string]bool                // Claim → 409
	poison    map[string]bool                // Claim → 422 (artifacts failed)
	state     map[string]workload.ClaimState // State responses
	notReady  map[string]bool                // Ready → false (default ready)
	requests  map[string]int64               // Requests responses
	claimed   []string                       // successful claim IPs, in order
	tokens    []string                       // bearer tokens presented with them
	lastClaim *workload.ClaimRequest
	// onReady runs on every serving-readiness poll, so a test can act at a
	// moment it could not otherwise reach: mid-wait, with the activation
	// published and its workload not answering yet.
	onReady func()
}

func newFakeClaims() *fakeClaims {
	return &fakeClaims{
		conflict: map[string]bool{},
		poison:   map[string]bool{},
		state:    map[string]workload.ClaimState{},
		notReady: map[string]bool{},
		requests: map[string]int64{},
	}
}

func (f *fakeClaims) Claim(_ context.Context, podIP, token string, req *workload.ClaimRequest) error {
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
	f.lastClaim = req
	return nil
}

func (f *fakeClaims) State(_ context.Context, podIP string) (*workload.ClaimState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.state[podIP]
	return &state, nil
}

func (f *fakeClaims) Ready(_ context.Context, podIP string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onReady != nil {
		f.onReady()
	}
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
	claims := newFakeClaims()
	o := wireOrchestrator(cs, gatewayfake.NewClientset(), cfg, func(w *warm.Config) {
		w.Client = claims
		w.Poll = time.Millisecond
		w.ColdWait = time.Second
		w.ServeWait = 50 * time.Millisecond
	})
	// Normally get-or-created from the pool-claim-key Secret on Start.
	seedClaimKey(t, cs)
	return o, cs, claims
}

// seedClaimKey pre-creates the claim-key Secret with a fixed key, so derived
// claim tokens are deterministic.
func seedClaimKey(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	_, err := cs.CoreV1().Secrets(testNS).Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: naming().SecretName, Namespace: testNS},
		Data:       map[string][]byte{"key": testInstallKey},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed claim key: %v", err)
	}
}

// warmPodFixture is a running, warm-ready pool pod as the replenisher would
// have produced it (labels, Ready condition, IP — no token anywhere: claim
// tokens are derived from the pod name).
func warmPodFixture(poolID, name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelPoolID:    poolID,
			},
			Annotations: map[string]string{},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  warm.ContainerWorkload,
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

func setWorkloadTerminated(pod *corev1.Pod, exitCode int32) {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: warm.ContainerWorkload,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: exitCode,
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

func testPool(id string) pool.Pool {
	return pool.Pool{ID: id, Image: "runtime:latest", Port: 8080, Size: 1, Burst: pool.BurstReject}
}

func TestActivate_ClaimConflictRetriesNextPod(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("std"))
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	addPod(t, cs, warmPodFixture("std", "pod-b", "10.0.0.2"))
	claims.conflict["10.0.0.1"] = true // a racing replica won pod-a

	status, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "serve"})
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

// A 422 poison claim fails the ACTIVATION, not the request — this backend
// used to surface it as a 500, diverging from Docker and the documented
// claim protocol until the shared claim flow unified them.
func TestActivate_PoisonedClaimReportsFailedActivation(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("std"))
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	claims.poison["10.0.0.1"] = true

	status, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "serve"})
	if err != nil {
		t.Fatalf("Activate: %v (poison must not be an error)", err)
	}
	if status.State != pool.StateFailed {
		t.Errorf("state = %s, want failed", status.State)
	}
	if status.PodID != "pod-a" {
		t.Errorf("PodID = %s, want the poisoned pod", status.PodID)
	}
	if !strings.Contains(status.Error, "artifacts failed") {
		t.Errorf("error = %q, want the sidecar's artifact failure", status.Error)
	}
}

func TestActivate_BurstRejectIsExhausted(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, testPool("std"))

	_, err := o.Activate(t.Context(), "std", &pool.Activation{Command: "serve"})
	if !errors.Is(err, apperrors.ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	if got := apperrors.HTTPStatus(err); got != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus: want 429, got %d", got)
	}
}

func TestActivate_BurstColdCreatesPod(t *testing.T) {
	t.Parallel()
	p := testPool("std")
	p.Burst = pool.BurstCold
	o, cs, claims := newTestOrchestrator(t, p)
	// Make every created pod immediately warm-ready, as the kubelet
	// eventually would.
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.9.9",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		}
		return false, nil, nil
	})

	status, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "serve"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.State != pool.StateReady || status.URL != "http://act1.pools.example.com" {
		t.Errorf("want ready at http://act1.pools.example.com, got %s %q", status.State, status.URL)
	}
	if len(claims.claimed) != 1 || claims.claimed[0] != "10.0.9.9" {
		t.Errorf("want the cold pod claimed, got %v", claims.claimed)
	}
}

func TestActivate_HTTPCreatesServiceAndRoute(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("web"))
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
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != int(workload.DefaultProxyPort) {
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
	o, cs, _ := newTestOrchestrator(t, testPool("web"))
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
	o, cs, claims := newTestOrchestrator(t, testPool("web"))
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
	o, cs, _ := newTestOrchestrator(t, testPool("std"))
	addPod(t, cs, claimedPodFixture("std", "pod-a", "10.0.0.1", "act1", pool.Activation{Command: "run"}))

	_, err := o.Activate(t.Context(), "std", &pool.Activation{ID: "act1", Command: "run"})
	if !errors.Is(err, apperrors.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestActivate_UnknownPoolNotFound(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, testPool("std"))
	_, err := o.Activate(t.Context(), "nope", &pool.Activation{Command: "run"})
	if !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStatusFromPod_Derivation(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, testPool("web"))
	web := o.warm.Pool("web")

	serving := claimedPodFixture("web", "pod-a", "10.0.0.1", "act1", pool.Activation{Command: "serve"})

	starting := claimedPodFixture("web", "pod-b", "10.0.0.2", "act2", pool.Activation{Command: "serve"})
	starting.Status.Conditions = nil // not yet serving-ready

	exited := claimedPodFixture("web", "pod-c", "10.0.0.3", "act3", pool.Activation{Command: "serve"})
	setWorkloadTerminated(exited, 3)

	infraFailed := claimedPodFixture("web", "pod-d", "10.0.0.4", "act4", pool.Activation{Command: "serve"})
	infraFailed.Status.Phase = corev1.PodFailed
	infraFailed.Status.ContainerStatuses = nil
	infraFailed.Status.Reason = "Evicted"

	deleting := claimedPodFixture("web", "pod-e", "10.0.0.5", "act5", pool.Activation{Command: "serve"})
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantState string
		wantError string
	}{
		{"serving-ready is ready", serving, pool.StateReady, ""},
		{"not yet ready is activating", starting, pool.StateActivating, ""},
		{"workload exit is failed", exited, pool.StateFailed, "workload exited with code 3"},
		{"infra failure is failed", infraFailed, pool.StateFailed, "Evicted"},
		{"deletion in flight is deactivating", deleting, pool.StateDeactivating, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := o.statusFromPod(web, tt.pod)
			if got.State != tt.wantState {
				t.Errorf("state: want %s, got %s (error %q)", tt.wantState, got.State, got.Error)
			}
			if tt.wantError != "" && !strings.Contains(got.Error, tt.wantError) {
				t.Errorf("error: want %q, got %q", tt.wantError, got.Error)
			}
			if got.URL == "" {
				t.Error("want an URL on every activation")
			}
		})
	}
}

func TestStatus_ReconstructsFromAnnotation(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("web"))
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
	o, _, _ := newTestOrchestrator(t, testPool("std"))
	if _, err := o.Status(t.Context(), "std", "ghost"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestList_ReturnsClaimedPodsOnly(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("std"))
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
	p := testPool("std")
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
	o, cs, _ := newTestOrchestrator(t, testPool("web"))
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

func TestBindPod_StripsCallbackKeyFromAnnotation(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("web"))
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
	o, _, _ := newTestOrchestrator(t, testPool("std"))
	if err := o.Deactivate(t.Context(), "std", "ghost"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// A client that hangs up during the serving wait cancels the context the
// activation is riding on. The pod is claimed and labeled by then — permanently
// out of the warm set — and its Service and route are published, so returning
// the error alone would leave a whole activation nobody asked for and nobody
// knows the id of.
func TestActivate_TearsDownWhenTheCallerGoesAwayMidServingWait(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))
	ctx, cancel := context.WithCancel(t.Context())
	claims.notReady["10.0.0.1"] = true // the workload never answers
	claims.onReady = cancel            // ...and the client gives up while we wait

	if _, err := o.Activate(ctx, "web", &pool.Activation{ID: "site", Command: "serve"}); err == nil {
		t.Fatal("a cancelled activation must fail")
	}

	if !podGone(t, cs, "pod-a") {
		t.Error("the claimed pod must be discarded: labeled, it will never count as warm again")
	}
	if _, err := cs.CoreV1().Services(testNS).Get(t.Context(), "act-site", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("want the service torn down, got %v", err)
	}
	if _, err := o.gateway.GatewayV1().HTTPRoutes(testNS).Get(t.Context(), "act-site", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("want the route torn down, got %v", err)
	}
}

// The publish step itself can fail — a conflicting Service, an API server that
// went away — and it happens with the pod already claimed and labeled. What was
// published before the failure goes with it.
func TestActivate_TearsDownWhenPublishingFails(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))
	cs.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("service create refused")
	})

	if _, err := o.Activate(t.Context(), "web", &pool.Activation{ID: "site", Command: "serve"}); err == nil {
		t.Fatal("a failed publish must fail the activation")
	}
	if !podGone(t, cs, "pod-a") {
		t.Error("the claimed pod must not be left behind by a failed publish")
	}
}

// The duplicate-id guard is a read taken before the claim, so two concurrent
// creates carrying the same activation id can both get past it and both find the
// routing objects already published. If one of them then fails, its teardown
// must not take the other's URL down with it — the surviving activation is still
// running and still expects to be reachable.
func TestActivate_TeardownLeavesAnotherRequestsObjectsAlone(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("web"))
	addPod(t, cs, warmPodFixture("web", "pod-a", "10.0.0.1"))

	// The first request published these and is serving on them.
	if _, err := o.createActivationService(t.Context(), "web", "site"); err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, err := o.createActivationRoute(t.Context(), "web", "site", "site.pools.example.com"); err != nil {
		t.Fatalf("route: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	claims.notReady["10.0.0.1"] = true
	claims.onReady = cancel

	if _, err := o.Activate(ctx, "web", &pool.Activation{ID: "site", Command: "serve"}); err == nil {
		t.Fatal("a cancelled activation must fail")
	}

	// Its own pod is its own business.
	if !podGone(t, cs, "pod-a") {
		t.Error("the failed activation's pod must still be discarded")
	}
	if _, err := cs.CoreV1().Services(testNS).Get(t.Context(), "act-site", metav1.GetOptions{}); err != nil {
		t.Errorf("the other request's service must survive: %v", err)
	}
	if _, err := o.gateway.GatewayV1().HTTPRoutes(testNS).Get(t.Context(), "act-site", metav1.GetOptions{}); err != nil {
		t.Errorf("the other request's route must survive: %v", err)
	}
}
