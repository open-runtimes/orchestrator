package warm

import (
	"orchestrator/internal/proxy"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	sidecar.state["10.0.0.1"] = proxy.ClaimState{Claimed: true, Failed: true, Error: "artifacts failed"}

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
	sidecar.state["10.0.0.1"] = proxy.ClaimState{Claimed: true, ActivationID: "lost"}

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
