package warm

import (
	"context"
	"orchestrator/internal/pool"
	"orchestrator/internal/workload"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// unlabeledPods counts the pool's pods without a claim label.
func unlabeledPods(t *testing.T, cs *fake.Clientset, poolID string) int {
	t.Helper()
	list, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{
		LabelSelector: testNaming.Pool + "=" + poolID + ",!" + testNaming.Claim,
	})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return len(list.Items)
}

func TestReconcile_DiscardsAbandonedReservation(t *testing.T) {
	t.Parallel()
	p := testPool("std")
	m, cs, _ := newTestManager(t, p)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	pod := claimedPodFixture(m, "std", "pod-a", "10.0.0.1", "act1", "{}")
	pod.Annotations[AnnotationReservedAt] = now.Add(-2 * m.cfg.OrphanTTL).Format(time.RFC3339Nano)
	addPod(t, cs, pod)

	c := m.Controller(Hooks{})
	c.Now = func() time.Time { return now }
	c.Tick(t.Context())

	if !podGone(t, cs, pod.Name) {
		t.Fatal("abandoned pre-activation reservation was not discarded")
	}
}

func TestClaimControl_AcceptsReservationConfirmedBySidecar(t *testing.T) {
	t.Parallel()
	m, cs, sidecar := newTestManager(t, testPool("std"))
	pod := claimedPodFixture(m, "std", "pod-a", "10.0.0.1", "act1", "{}")
	pod.Annotations[AnnotationReservedAt] = time.Now().Add(-2 * m.cfg.OrphanTTL).Format(time.RFC3339Nano)
	addPod(t, cs, pod)
	sidecar.state[pod.Status.PodIP] = workload.ClaimState{Claimed: true, ClaimID: "act1"}

	called := 0
	c := m.Controller(Hooks{Reap: func(context.Context, *pool.Pool, *corev1.Pod, string, time.Time) { called++ }})
	c.TickClaims(t.Context())

	if called != 1 || podGone(t, cs, pod.Name) {
		t.Fatalf("confirmed reservation: reap calls=%d gone=%v, want 1/false", called, podGone(t, cs, pod.Name))
	}
}

func TestReplenish_CreatesUpToSize(t *testing.T) {
	t.Parallel()
	p := testPool("std")
	p.Size = 3
	m, cs, _ := newTestManager(t, p)
	addPod(t, cs, warmPodFixture(m, "std", "pod-a", "10.0.0.1"))
	// Claimed pods must not count toward the warm size.
	addPod(t, cs, claimedPodFixture(m, "std", "pod-b", "10.0.0.2", "act1", `{"command":"run"}`))

	m.Controller(Hooks{}).Tick(t.Context())

	if got := unlabeledPods(t, cs, "std"); got != 3 {
		t.Errorf("want 3 warm pods after replenish, got %d", got)
	}
}

func TestReplenish_CountsPendingPods(t *testing.T) {
	t.Parallel()
	p := testPool("std")
	p.Size = 2
	m, cs, _ := newTestManager(t, p)
	addPod(t, cs, warmPodFixture(m, "std", "pod-a", "10.0.0.1"))
	pending := warmPodFixture(m, "std", "pod-b", "")
	pending.Status = corev1.PodStatus{Phase: corev1.PodPending} // on its way: counts, no over-creation
	addPod(t, cs, pending)

	m.Controller(Hooks{}).Tick(t.Context())

	if got := unlabeledPods(t, cs, "std"); got != 2 {
		t.Errorf("want no new pods (2 counted), got %d", got)
	}
}

func TestReplenish_DeletesPoisonedPods(t *testing.T) {
	t.Parallel()
	m, cs, sidecar := newTestManager(t, testPool("std"))
	addPod(t, cs, warmPodFixture(m, "std", "pod-a", "10.0.0.1"))
	sidecar.state["10.0.0.1"] = workload.ClaimState{Claimed: true, Failed: true, Error: "artifacts failed"}

	m.Controller(Hooks{}).Tick(t.Context())

	if !podGone(t, cs, "pod-a") {
		t.Error("want the poisoned pod deleted")
	}
	if got := unlabeledPods(t, cs, "std"); got != 1 {
		t.Errorf("want a replacement created, got %d warm pods", got)
	}
}

func TestOrphanGC_DiscardsAfterTTL(t *testing.T) {
	t.Parallel()
	m, cs, sidecar := newTestManager(t, testPool("std"))
	// Claimed by the sidecar's own account, but never labeled: the service
	// crashed between accept and patch.
	addPod(t, cs, warmPodFixture(m, "std", "pod-a", "10.0.0.1"))
	sidecar.state["10.0.0.1"] = workload.ClaimState{Claimed: true, ClaimID: "lost"}

	c := m.Controller(Hooks{})
	t0 := time.Now()
	c.Now = func() time.Time { return t0 }
	c.Tick(t.Context())
	if podGone(t, cs, "pod-a") {
		t.Fatal("orphan must survive until the TTL elapses")
	}

	c.Now = func() time.Time { return t0.Add(61 * time.Second) }
	c.Tick(t.Context())
	if !podGone(t, cs, "pod-a") {
		t.Error("want the orphan discarded after the TTL — never resold")
	}
}

// A claim whose pool is not configured — a workload that brought its own pool of
// one — is still reaped when it goes idle. No declared pool's reconcile would
// ever see it, so without this it would hold its pod forever.
func TestTick_ReapsClaimsWithNoConfiguredPool(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset()
	m := New(cs, []pool.Pool{{ID: "declared", Size: 1, Spec: pool.Spec{Image: "img"}}}, Config{
		Namespace: testNS, Naming: testNaming, ReapUnpooled: true, Client: newFakeSidecar(),
	})

	// One pod in a declared pool, one belonging to a pool that is not configured.
	for _, tc := range []struct{ name, poolID, claimID string }{
		{"pool-declared-aaaaa", "declared", "act-1"},
		{"pool-solo-bbbbb", "solo", "sbx-1"},
	} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      tc.name,
			Namespace: testNS,
			Labels: map[string]string{
				LabelManagedBy:   testNaming.ManagedBy,
				testNaming.Pool:  tc.poolID,
				testNaming.Claim: tc.claimID,
			},
		}}
		if _, err := cs.CoreV1().Pods(testNS).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}

	var reaped []string
	c := m.Controller(Hooks{Reap: func(_ context.Context, _ *pool.Pool, _ *corev1.Pod, claimID string, _ time.Time) {
		reaped = append(reaped, claimID)
	}})
	c.Tick(context.Background())

	slices.Sort(reaped)
	if len(reaped) != 2 || reaped[0] != "act-1" || reaped[1] != "sbx-1" {
		t.Errorf("both claims must be offered to the reaper, got %v", reaped)
	}
}

// A consumer that only claims from declared pools leaves the sweep off, so
// removing a pool from the config cannot start reaping its live claims.
func TestTick_LeavesUnpooledClaimsAloneWhenNotOptedIn(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset()
	m := New(cs, nil, Config{Namespace: testNS, Naming: testNaming, Client: newFakeSidecar()})

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "pool-gone-aaaaa",
		Namespace: testNS,
		Labels: map[string]string{
			LabelManagedBy:   testNaming.ManagedBy,
			testNaming.Pool:  "removed-from-config",
			testNaming.Claim: "act-1",
		},
	}}
	if _, err := cs.CoreV1().Pods(testNS).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	var reaped []string
	c := m.Controller(Hooks{Reap: func(_ context.Context, _ *pool.Pool, _ *corev1.Pod, claimID string, _ time.Time) {
		reaped = append(reaped, claimID)
	}})
	c.Tick(context.Background())

	if len(reaped) != 0 {
		t.Errorf("nothing should have been reaped, got %v", reaped)
	}
}

func TestTick_CleansRemovedPoolWarmPodsWithoutReapingClaims(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset()
	m := New(cs, nil, Config{
		Namespace: testNS, Naming: testNaming, Client: newFakeSidecar(),
		ColdWait: time.Second, OrphanTTL: time.Second,
	})
	now := time.Now()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-removed-warm", Namespace: testNS,
		Labels: m.PoolLabels("removed"), CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
	}}
	if _, err := cs.CoreV1().Pods(testNS).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	c := m.Controller(Hooks{})
	c.Now = func() time.Time { return now }
	c.Tick(t.Context())
	if _, err := cs.CoreV1().Pods(testNS).Get(t.Context(), pod.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("warm pod from removed pool was not deleted: %v", err)
	}
}

func TestTick_NeverSweepsDirectWorkloadWithoutPoolLabel(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset()
	m := New(cs, nil, Config{
		Namespace: testNS, Naming: testNaming, Client: newFakeSidecar(),
		ColdWait: time.Second, OrphanTTL: time.Second,
	})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "direct-revision-pod", Namespace: testNS,
		Labels:            map[string]string{LabelManagedBy: testNaming.ManagedBy},
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	if _, err := cs.CoreV1().Pods(testNS).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	m.Controller(Hooks{}).Tick(t.Context())
	if _, err := cs.CoreV1().Pods(testNS).Get(t.Context(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("direct workload entered pool cleanup: %v", err)
	}
}

func TestTickClaims_DoesNotReconcileBareInventory(t *testing.T) {
	t.Parallel()
	p := testPool("std")
	p.Size = 2
	m, cs, _ := newTestManager(t, p)

	m.Controller(Hooks{}).TickClaims(t.Context())
	if got := unlabeledPods(t, cs, p.ID); got != 0 {
		t.Fatalf("consumer lifecycle created %d warm pods; inventory belongs to pool-controller", got)
	}
}

func TestTick_ReplacesUnclaimedPodAfterPoolSpecChange(t *testing.T) {
	t.Parallel()
	old := testPool("changed")
	old.Size = 1
	old.Image = "old-image"
	current := old
	current.Image = "new-image"
	m, cs, _ := newTestManager(t, current)
	stale := warmPodFixture(New(cs, []pool.Pool{old}, Config{
		Namespace: testNS, Naming: testNaming, Client: newFakeSidecar(),
	}), old.ID, "pool-changed-stale", "10.0.0.8")
	addPod(t, cs, stale)

	m.Controller(Hooks{}).Tick(t.Context())
	if _, err := cs.CoreV1().Pods(testNS).Get(t.Context(), stale.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale warm pod was not deleted: %v", err)
	}
	pods, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{LabelSelector: testNaming.Pool + "=changed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Spec.Containers[0].Image != "new-image" {
		t.Fatalf("replacement does not use current pool spec: %#v", pods.Items)
	}
}

// A pod created for one request and never claimed is invisible to every other
// loop: its pool id is the request's, which no config declares, and the unpooled
// reaper lists only pods that carry a claim. Without this sweep it would hold its
// CPU and memory until someone noticed by hand.
func TestSweepUnclaimed_DiscardsAbandonedPoollessPods(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset()
	m := New(cs, []pool.Pool{{ID: "declared", Size: 1, Spec: pool.Spec{Image: "img"}}}, Config{
		Namespace: testNS, Naming: testNaming, ReapUnpooled: true, Client: newFakeSidecar(),
		ColdWait: 120 * time.Second, OrphanTTL: 60 * time.Second,
	})
	now := time.Now()

	// Three unclaimed pods: one abandoned poolless pod well past the threshold,
	// one still inside it (a create that may yet succeed), and a warm pod of a
	// declared pool, which its own reconcile owns.
	for _, tc := range []struct {
		name, poolID string
		age          time.Duration
	}{
		{"pool-solo-aaaaa", "solo", 10 * time.Minute},
		{"pool-fresh-aaaaa", "fresh", 30 * time.Second},
		{"pool-declared-aaaaa", "declared", 10 * time.Minute},
	} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:              tc.name,
			Namespace:         testNS,
			Labels:            m.PoolLabels(tc.poolID),
			CreationTimestamp: metav1.NewTime(now.Add(-tc.age)),
		}}
		if _, err := cs.CoreV1().Pods(testNS).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", tc.name, err)
		}
	}

	c := m.Controller(Hooks{})
	c.Now = func() time.Time { return now }
	c.sweepUnclaimed(t.Context())

	live := map[string]bool{}
	pods, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := range pods.Items {
		live[pods.Items[i].Name] = true
	}
	if live["pool-solo-aaaaa"] {
		t.Error("the abandoned poolless pod must be discarded — nothing else can see it")
	}
	if !live["pool-fresh-aaaaa"] {
		t.Error("a pod still inside the cold-start window may be mid-claim; deleting it fails a live create")
	}
	if !live["pool-declared-aaaaa"] {
		t.Error("a declared pool's warm pod is its reconcile's business, not the sweep's")
	}
}
