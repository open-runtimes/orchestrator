package kubernetes

import (
	"orchestrator/internal/proxy"
	"orchestrator/pkg/pool"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// unlabeledPods counts the pool's pods without an activation label.
func unlabeledPods(t *testing.T, cs *fake.Clientset, poolID string) int {
	t.Helper()
	list, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{
		LabelSelector: LabelPoolID + "=" + poolID + ",!" + LabelActivation,
	})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return len(list.Items)
}

func TestReplenish_CreatesUpToSize(t *testing.T) {
	t.Parallel()
	p := execPool("std")
	p.Size = 3
	o, cs, _ := newTestOrchestrator(t, p)
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	// Claimed pods must not count toward the warm size.
	addPod(t, cs, claimedPodFixture("std", "pod-b", "10.0.0.2", "act1", pool.Activation{Command: "run"}))

	newController(o).tick(t.Context())

	if got := unlabeledPods(t, cs, "std"); got != 3 {
		t.Errorf("want 3 warm pods after replenish, got %d", got)
	}
}

func TestReplenish_CountsPendingPods(t *testing.T) {
	t.Parallel()
	p := execPool("std")
	p.Size = 2
	o, cs, _ := newTestOrchestrator(t, p)
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	pending := warmPodFixture("std", "pod-b", "")
	pending.Status = corev1.PodStatus{Phase: corev1.PodPending} // on its way: counts, no over-creation
	addPod(t, cs, pending)

	newController(o).tick(t.Context())

	if got := unlabeledPods(t, cs, "std"); got != 2 {
		t.Errorf("want no new pods (2 counted), got %d", got)
	}
}

func TestReplenish_DeletesPoisonedPods(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, execPool("std"))
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	claims.state["10.0.0.1"] = proxy.ClaimState{Claimed: true, Failed: true, Error: "artifacts failed"}

	newController(o).tick(t.Context())

	if !podGone(t, cs, "pod-a") {
		t.Error("want the poisoned pod deleted")
	}
	if got := unlabeledPods(t, cs, "std"); got != 1 {
		t.Errorf("want a replacement created, got %d warm pods", got)
	}
}

func TestOrphanGC_DiscardsAfterTTL(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, execPool("std"))
	o.cfg.OrphanTTL = 30 * time.Second
	// Claimed by the sidecar's own account, but never labeled: the service
	// crashed between accept and patch.
	addPod(t, cs, warmPodFixture("std", "pod-a", "10.0.0.1"))
	claims.state["10.0.0.1"] = proxy.ClaimState{Claimed: true, ActivationID: "lost"}

	c := newController(o)
	t0 := time.Now()
	c.now = func() time.Time { return t0 }
	c.tick(t.Context())
	if podGone(t, cs, "pod-a") {
		t.Fatal("orphan must survive until the TTL elapses")
	}

	c.now = func() time.Time { return t0.Add(31 * time.Second) }
	c.tick(t.Context())
	if !podGone(t, cs, "pod-a") {
		t.Error("want the orphan discarded after the TTL — never resold")
	}
}

func TestRetentionGC_ReapsFinishedExecPods(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t, execPool("std"))
	t0 := time.Now()

	old := claimedPodFixture("std", "pod-old", "10.0.0.1", "act1", pool.Activation{Command: "run"})
	setWorkloadTerminated(old, 0, t0.Add(-20*time.Minute))
	addPod(t, cs, old)

	recent := claimedPodFixture("std", "pod-recent", "10.0.0.2", "act2", pool.Activation{Command: "run"})
	setWorkloadTerminated(recent, 0, t0.Add(-5*time.Minute))
	addPod(t, cs, recent)

	c := newController(o)
	c.now = func() time.Time { return t0 }
	c.tick(t.Context())

	if !podGone(t, cs, "pod-old") {
		t.Error("want the pod past ActivationRetention reaped")
	}
	if podGone(t, cs, "pod-recent") {
		t.Error("want the recently finished pod kept for Status")
	}
}

func TestIdleTeardown_DeactivatesAfterNoRequestDelta(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, claimedPodFixture("web", "pod-a", "10.0.0.1", "site",
		pool.Activation{Command: "serve", IdleTimeoutSeconds: 60}))
	claims.requests["10.0.0.1"] = 5

	c := newController(o)
	t0 := time.Now()
	c.now = func() time.Time { return t0 }
	c.tick(t.Context()) // records the baseline

	c.now = func() time.Time { return t0.Add(61 * time.Second) }
	c.tick(t.Context()) // no delta across the window → deactivate

	if !podGone(t, cs, "pod-a") {
		t.Error("want the idle activation torn down")
	}
}

func TestIdleTeardown_TrafficResetsTheClock(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, claimedPodFixture("web", "pod-a", "10.0.0.1", "site",
		pool.Activation{Command: "serve", IdleTimeoutSeconds: 60}))
	claims.requests["10.0.0.1"] = 5

	c := newController(o)
	t0 := time.Now()
	c.now = func() time.Time { return t0 }
	c.tick(t.Context())

	claims.mu.Lock()
	claims.requests["10.0.0.1"] = 6 // a request landed
	claims.mu.Unlock()
	c.now = func() time.Time { return t0.Add(61 * time.Second) }
	c.tick(t.Context())

	if podGone(t, cs, "pod-a") {
		t.Error("activation with fresh traffic must not be torn down")
	}
}

func TestIdleTeardown_ZeroIdleTimeoutMeansUntilDelete(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, httpPool("web"))
	addPod(t, cs, claimedPodFixture("web", "pod-a", "10.0.0.1", "site",
		pool.Activation{Command: "serve"}))
	claims.requests["10.0.0.1"] = 0

	c := newController(o)
	t0 := time.Now()
	c.now = func() time.Time { return t0 }
	c.tick(t.Context())
	c.now = func() time.Time { return t0.Add(24 * time.Hour) }
	c.tick(t.Context())

	if podGone(t, cs, "pod-a") {
		t.Error("IdleTimeoutSeconds 0 must never idle out")
	}
}
