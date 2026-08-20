package kubernetes

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/pool"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/volume"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"
	"slices"
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
)

const testNS = "orchestrator"

// fakeSidecar fakes the sidecar surface per pod IP: fake-clientset pods have no
// reachable sidecars.
type fakeSidecar struct {
	mu       sync.Mutex
	poison   map[string]bool
	notReady map[string]bool
	requests map[string]int64
	last     *workload.ClaimRequest
	// onReady runs on every readiness poll, so a test can act at a moment it
	// could not otherwise reach — mid-wait, with the pod claimed and serving
	// nothing yet.
	onReady func()
}

func newFakeSidecar() *fakeSidecar {
	return &fakeSidecar{
		poison:   map[string]bool{},
		notReady: map[string]bool{},
		requests: map[string]int64{},
	}
}

func (f *fakeSidecar) Claim(_ context.Context, podIP, _ string, req *workload.ClaimRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.poison[podIP] {
		return &claim.Poison{Msg: "artifacts failed: boom"}
	}
	f.last = req
	return nil
}

func (f *fakeSidecar) State(_ context.Context, podIP string) (*workload.ClaimState, error) {
	return &workload.ClaimState{}, nil
}

func (f *fakeSidecar) Ready(_ context.Context, podIP string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onReady != nil {
		f.onReady()
	}
	return !f.notReady[podIP]
}

func (f *fakeSidecar) Requests(_ context.Context, podIP string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[podIP], nil
}

// testPool is a pool over a plain runtime image: it names no command, so the
// sandbox runs the agent the shim installs — the ordinary case.
func testPool(id string) pool.Pool {
	return pool.Pool{ID: id, Size: 1, Burst: pool.BurstReject, Spec: pool.Spec{Image: "node:22-slim", Port: 3000}}
}

func newTestOrchestrator(t *testing.T, pools ...pool.Pool) (*Orchestrator, *fake.Clientset, *fakeSidecar) {
	t.Helper()
	cs := fake.NewClientset()
	cfg := Config{
		SidecarImage:  "sidecar:latest",
		ShimImage:     "shim:latest",
		Namespace:     testNS,
		RunAsUser:     65532,
		SandboxDomain: "sandboxes.example.com",
		Pools:         pools,
	}
	cfg.applyDefaults()
	sidecar := newFakeSidecar()
	o := wireOrchestrator(cs, cfg, func(w *warm.Config) {
		w.Client = sidecar
		w.Poll = time.Millisecond
		w.ColdWait = time.Second
		w.ServeWait = 50 * time.Millisecond
	})
	return o, cs, sidecar
}

// warmPodFixture is a running, warm-ready sandbox pool pod as the replenisher
// would have produced it.
func warmPodFixture(o *Orchestrator, poolID, name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNS,
			Labels:      map[string]string{warm.LabelManagedBy: ManagedByValue, LabelPoolID: poolID},
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

func ptrTo[T any](v T) *T { return &v }

func request(id string) *sandbox.Request {
	return &sandbox.Request{ID: id, Pool: "py", Token: "9f3c1a04b7e28d65f1024c8ba3e7d95f", TimeoutSeconds: ptrTo(300)}
}

func TestCreate_ClaimsAndStampsTheToken(t *testing.T) {
	t.Parallel()
	o, cs, sidecar := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))

	status, err := o.Create(t.Context(), request("agent"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if status.State != sandbox.StateReady {
		t.Fatalf("state: want ready, got %s (%s)", status.State, status.Error)
	}
	// The URL carries the token, never the caller-chosen id: an id is
	// guessable, and reaching the URL is code execution.
	want := "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com"
	if status.URL != want {
		t.Errorf("url: want %s, got %s", want, status.URL)
	}
	if strings.Contains(status.URL, "agent") {
		t.Error("the sandbox id must not appear in its address")
	}

	pod := getPod(t, cs, "sbx-py-aaaaa")
	if pod.Labels[LabelSandboxID] != "agent" {
		t.Errorf("sandbox id label: got %v", pod.Labels)
	}
	if pod.Labels[LabelToken] != "9f3c1a04b7e28d65f1024c8ba3e7d95f" {
		t.Errorf("token label (the edge's routing key): got %v", pod.Labels)
	}
	// With no command named anywhere, the claim execs the installed agent — this
	// is what lets an ordinary runtime image serve the sandbox contract.
	if sidecar.last.Command != AgentCommand() || sidecar.last.Port != 3000 {
		t.Errorf("claim request: got %+v", sidecar.last)
	}
}

func TestCreate_DuplicateIDConflicts(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-bbbbb", "10.0.0.2"))

	if _, err := o.Create(t.Context(), request("agent")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := o.Create(t.Context(), request("agent"))
	if !errors.Is(err, apperrors.ErrConflict) {
		t.Fatalf("want ErrConflict for a re-used id, got %v", err)
	}
}

func TestCreate_PoisonedArtifactsFailWithoutURL(t *testing.T) {
	t.Parallel()
	o, cs, sidecar := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))
	sidecar.poison["10.0.0.1"] = true

	status, err := o.Create(t.Context(), request("agent"))
	if err != nil {
		t.Fatalf("artifact failure is a failed sandbox, not an error: %v", err)
	}
	if status.State != sandbox.StateFailed || !strings.Contains(status.Error, "artifacts failed") {
		t.Errorf("want a failed sandbox naming the reason, got %+v", status)
	}
	if status.URL != "" {
		t.Errorf("a failed sandbox must not hand out a URL, got %s", status.URL)
	}
}

func TestCreate_NeverServingTearsDown(t *testing.T) {
	t.Parallel()
	o, cs, sidecar := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))
	sidecar.notReady["10.0.0.1"] = true

	status, err := o.Create(t.Context(), request("agent"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if status.State != sandbox.StateFailed {
		t.Errorf("want failed, got %+v", status)
	}
	if !podGone(t, cs, "sbx-py-aaaaa") {
		t.Error("a sandbox that never served must not keep holding its pod")
	}
}

func TestStatusAndList_ReconstructFromPods(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))
	if _, err := o.Create(t.Context(), request("agent")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Nothing is held in memory: a fresh orchestrator over the same cluster
	// sees the same sandbox (the restart-survival property).
	fresh, _, _ := newTestOrchestrator(t, testPool("py"))
	fresh.warm = o.warm
	status, err := fresh.Status(t.Context(), "agent")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != sandbox.StateReady || status.PoolID != "py" {
		t.Errorf("status: got %+v", status)
	}
	if status.URL == "" {
		t.Error("status must carry the URL — the caller cannot derive it from the id")
	}
	// A claimed sandbox reports the shape of the pool it came from, so a caller
	// that never saw the operator's config can still see what it is running in.
	if status.Image != "node:22-slim" {
		t.Errorf("image: got %q, want the pool's", status.Image)
	}

	list, err := o.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != "agent" {
		t.Errorf("list: got %+v", list)
	}
}

func TestShape_CreateMatchesReadAndSurvivesThePool(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))

	created, err := o.Create(t.Context(), request("agent"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A create that reports less than a read of the same sandbox would make the
	// POST response the odd one out.
	if created.Image != "node:22-slim" {
		t.Errorf("create image: got %q, want the pool's", created.Image)
	}

	read, err := o.Status(t.Context(), "agent")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if read.Image != created.Image || read.CPU != created.CPU || read.Memory != created.Memory {
		t.Errorf("read shape %+v does not match created %+v", read, created)
	}

	// Re-image the pool under the running sandbox. The pod did not change, so
	// neither may its reported shape.
	repooled, _, _ := newTestOrchestrator(t, pool.Pool{
		ID: "py", Size: 1, Burst: pool.BurstReject,
		Spec: pool.Spec{Image: "node:24-slim", Port: 3000},
	})
	repooled.warm = o.warm
	after, err := repooled.Status(t.Context(), "agent")
	if err != nil {
		t.Fatalf("Status after re-image: %v", err)
	}
	if after.Image != "node:22-slim" {
		t.Errorf("image: got %q, want the image the pod is running", after.Image)
	}

	// And with the pool gone entirely, the shape is still the pod's.
	unpooled, _, _ := newTestOrchestrator(t)
	unpooled.warm = o.warm
	orphan, err := unpooled.Status(t.Context(), "agent")
	if err != nil {
		t.Fatalf("Status without pool: %v", err)
	}
	if orphan.Image != "node:22-slim" {
		t.Errorf("image: got %q, want the image the pod is running", orphan.Image)
	}
}

func TestStatus_UnknownSandboxNotFound(t *testing.T) {
	t.Parallel()
	o, _, _ := newTestOrchestrator(t, testPool("py"))
	if _, err := o.Status(t.Context(), "nope"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDelete_DiscardsThePodAndItsToken(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))
	if _, err := o.Create(t.Context(), request("agent")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := o.Delete(t.Context(), "agent"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The token lived only as a label on that pod, so a leaked URL is dead.
	if !podGone(t, cs, "sbx-py-aaaaa") {
		t.Error("want the pod deleted")
	}
	if err := o.Delete(t.Context(), "agent"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("want ErrNotFound on a second delete, got %v", err)
	}
}

func TestDelete_IdleSandboxTornDownByTheControlLoop(t *testing.T) {
	t.Parallel()
	o, cs, sidecar := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))
	req := request("agent")
	req.IdleTimeoutSeconds = 60
	if _, err := o.Create(t.Context(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sidecar.requests["10.0.0.1"] = 3

	c := o.warm.Controller(warm.NewIdleReaper(o.warm, func(ctx context.Context, _, id string) error {
		return o.Delete(ctx, id)
	}).Hooks())
	t0 := time.Now()
	c.Now = func() time.Time { return t0 }
	c.Tick(t.Context()) // baseline
	c.Now = func() time.Time { return t0.Add(61 * time.Second) }
	c.Tick(t.Context()) // no traffic across the window → torn down

	if !podGone(t, cs, "sbx-py-aaaaa") {
		t.Error("want the abandoned sandbox reaped — it holds a warm pod hostage")
	}
}

// Two containers agree through the shared workspace: the agent-install init
// container copies the binary somewhere, and the claim execs it there. That has
// to be one value — a mismatch is not a compile error, it is a sandbox that
// starts and cannot exec — so the copy destination, the default command and the
// published source all come from pkg/sandbox.
func TestAgentContract_TheCopyDestinationIsWhatTheSandboxRuns(t *testing.T) {
	t.Parallel()
	cfg := Config{SandboxDomain: "sandboxes.test"}
	cfg.applyDefaults()
	agent := cfg.warmConfig().Agent

	// Nobody names a command: the pool image serves nothing, the agent does.
	req := claimRequest(&pool.Spec{Port: 3000}, &sandbox.Request{ID: "sbx"})
	if req.Command != agent.Dest {
		t.Errorf("the sandbox runs %q but the agent is copied to %q", req.Command, agent.Dest)
	}
	if AgentCommand() != agent.Dest {
		t.Errorf("AgentCommand is %q, the copy destination is %q", AgentCommand(), agent.Dest)
	}
	if agent.Source != sandbox.AgentSource {
		t.Errorf("copy source: want %q, got %q", sandbox.AgentSource, agent.Source)
	}
	// The default is pinned by tag: the tag is the version.
	if agent.Image != sandbox.AgentImage {
		t.Errorf("agent image: want %q, got %q", sandbox.AgentImage, agent.Image)
	}
	// The destination is inside the workspace both containers mount.
	if !strings.HasPrefix(agent.Dest, workspacePath+"/") {
		t.Errorf("%q is not in the shared workspace %q", agent.Dest, workspacePath)
	}
}

// The grammar itself is pkg/sandbox's (host_test.go); what this backend owns is
// that a gateway fronts port 80, so its URLs are bare.
func TestAddressing_GatewayFrontedURLsAreBare(t *testing.T) {
	t.Parallel()
	cfg := Config{SandboxDomain: "sandboxes.example.com", Scheme: "https"}
	if got := cfg.addressing().URL("abc"); got != "https://s-abc.sandboxes.example.com" {
		t.Errorf("URL: got %s", got)
	}
}

// Every port a sandbox serves is addressable, and the port shares the token's
// DNS label so one wildcard certificate covers them all.
func TestCreate_PortsGetTheirOwnHostnames(t *testing.T) {
	t.Parallel()
	o, cs, sidecar := newTestOrchestrator(t, testPool("py"))
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))

	req := request("agent")
	req.Ports = []int{5173}
	status, err := o.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := map[string]string{
		"3000": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com",
		"5173": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f-5173.sandboxes.example.com",
	}
	for port, url := range want {
		if status.URLs[port] != url {
			t.Errorf("urls[%s]: want %s, got %s", port, url, status.URLs[port])
		}
	}
	if status.URL != want["3000"] {
		t.Errorf("url must stay the primary port's: got %s", status.URL)
	}
	// The claim carries them, so the sidecar knows which ports may be reached.
	if len(sidecar.last.Ports) != 1 || sidecar.last.Ports[0] != 5173 {
		t.Errorf("claim ports: got %v", sidecar.last.Ports)
	}

	// And a reconstructed sandbox advertises the same set — the ports come off
	// the stored spec, not memory.
	reread, err := o.Status(t.Context(), "agent")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if reread.URLs["5173"] != want["5173"] {
		t.Errorf("reconstructed urls: got %v", reread.URLs)
	}
}

// A pool or request may still name a command — an image that serves the contract
// itself, or a wrapper around the agent — and it wins over the installed agent.
func TestCreate_CommandOverridesTheAgent(t *testing.T) {
	t.Parallel()
	p := testPool("py")
	p.Command = "/usr/local/bin/sandbox"
	o, cs, sidecar := newTestOrchestrator(t, p)
	addPod(t, cs, warmPodFixture(o, "py", "sbx-py-aaaaa", "10.0.0.1"))

	if _, err := o.Create(t.Context(), request("agent")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sidecar.last.Command != "/usr/local/bin/sandbox" {
		t.Errorf("pool command must win over the agent, got %q", sidecar.last.Command)
	}

	req := request("agent-2")
	req.Command = "node server.js"
	o2, cs2, sidecar2 := newTestOrchestrator(t, p)
	addPod(t, cs2, warmPodFixture(o2, "py", "sbx-py-bbbbb", "10.0.0.2"))
	if _, err := o2.Create(t.Context(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sidecar2.last.Command != "node server.js" {
		t.Errorf("request command must win over both, got %q", sidecar2.last.Command)
	}
}

// readyOnCreate makes every pod the code creates come up running and warm-ready,
// which no controller does behind a fake clientset. It is what lets the poolless
// path — create the pod, then claim it — be tested at all.
func readyOnCreate(cs *fake.Clientset, ip string) {
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  warm.ContainerWorkload,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		}
		return false, nil, nil
	})
}

// A poolless sandbox creates the one pod it needs and claims that. The pod takes
// its shape from the request — including storage a pool could never attach at
// claim time, because its pods are already running.
func TestCreate_PoollessTakesItsShapeFromTheRequest(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t)
	readyOnCreate(cs, "10.0.0.9")

	req := &sandbox.Request{
		ID: "solo", Token: "9f3c1a04b7e28d65f1024c8ba3e7d95f", TimeoutSeconds: ptrTo(300),
		Image: "python:3.12-slim", Port: 8000, CPU: 0.5, Memory: 256,
		Volumes: []volume.Volume{{Source: "tenant-pvc", Path: "/data", ReadOnly: true}},
	}
	status, err := o.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("poolless create: %v", err)
	}
	if status.State != sandbox.StateReady {
		t.Fatalf("state %q (%s)", status.State, status.Error)
	}
	if status.PoolID != "" {
		t.Errorf("there was no pool: got poolId %q", status.PoolID)
	}

	pods, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("want the one pod this sandbox needed, got %d (%v)", len(pods.Items), err)
	}
	pod := pods.Items[0]
	worker := pod.Spec.Containers[0]
	if worker.Image != "python:3.12-slim" {
		t.Errorf("worker image %q, want the request's", worker.Image)
	}
	if !slices.ContainsFunc(worker.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.MountPath == "/data" && m.ReadOnly
	}) {
		t.Errorf("the request's volume was not mounted into the worker: %+v", worker.VolumeMounts)
	}
	if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
		return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "tenant-pvc"
	}) {
		t.Errorf("the PVC was not attached to the pod: %+v", pod.Spec.Volumes)
	}
}

// The poolless path must not scan for warm capacity: nothing is standing in a
// shape that was described by this request, so a list is pure latency on the
// create path. The one list a create does make is the duplicate-id check.
func TestCreate_PoollessDoesNotScanForWarmCapacity(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t)
	readyOnCreate(cs, "10.0.0.9")

	if _, err := o.Create(t.Context(), &sandbox.Request{
		ID: "solo", Token: "9f3c1a04b7e28d65f1024c8ba3e7d95f", TimeoutSeconds: ptrTo(300),
		Image: "python:3.12-slim", Port: 8000,
	}); err != nil {
		t.Fatalf("poolless create: %v", err)
	}

	lists := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "pods" {
			lists++
		}
	}
	if lists != 1 {
		t.Errorf("want only the duplicate-id list, got %d pod lists", lists)
	}
}

// The window after the claim is the last one: the pod is bound and warming up
// its workload, and a client that hangs up here leaves it running. Await deletes
// a pod whose workload never serves in time, so stopping the wait early must do
// the same — a poolless sandbox has no idle ceiling to collect it, because it has
// no pool to declare one.
func TestCreate_DiscardsThePodWhenTheCallerGoesAwayMidReadiness(t *testing.T) {
	t.Parallel()
	o, cs, sidecar := newTestOrchestrator(t)
	readyOnCreate(cs, "10.0.0.9")
	ctx, cancel := context.WithCancel(t.Context())
	sidecar.notReady["10.0.0.9"] = true // the workload never answers
	sidecar.onReady = cancel            // ...and the client gives up while we wait

	if _, err := o.Create(ctx, &sandbox.Request{
		ID: "solo", Token: "9f3c1a04b7e28d65f1024c8ba3e7d95f", TimeoutSeconds: ptrTo(300),
		Image: "python:3.12-slim", Port: 8000,
	}); err == nil {
		t.Fatal("a cancelled create must fail")
	}

	pods, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("the claimed pod must be torn down, got %d still running", len(pods.Items))
	}
}
