package kubernetes

import (
	"orchestrator/internal/warm"
	"orchestrator/pkg/pool"
	"testing"
	"time"
)

// idleLoop is the warm control loop with the activation idle rule installed —
// the wiring Start uses, driven by a fake clock.
func idleLoop(o *Orchestrator) *warm.Controller {
	return o.warm.Controller(o.idleRule().Hooks())
}

func TestIdleTeardown_DeactivatesAfterNoRequestDelta(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("web"))
	addPod(t, cs, claimedPodFixture("web", "pod-a", "10.0.0.1", "site",
		pool.Activation{Command: "serve", IdleTimeoutSeconds: 60}))
	claims.requests["10.0.0.1"] = 5

	c := idleLoop(o)
	t0 := time.Now()
	c.Now = func() time.Time { return t0 }
	c.Tick(t.Context()) // records the baseline

	c.Now = func() time.Time { return t0.Add(61 * time.Second) }
	c.Tick(t.Context()) // no delta across the window → deactivate

	if !podGone(t, cs, "pod-a") {
		t.Error("want the idle activation torn down")
	}
}

func TestIdleTeardown_TrafficResetsTheClock(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("web"))
	addPod(t, cs, claimedPodFixture("web", "pod-a", "10.0.0.1", "site",
		pool.Activation{Command: "serve", IdleTimeoutSeconds: 60}))
	claims.requests["10.0.0.1"] = 5

	c := idleLoop(o)
	t0 := time.Now()
	c.Now = func() time.Time { return t0 }
	c.Tick(t.Context())

	claims.mu.Lock()
	claims.requests["10.0.0.1"] = 6 // a request landed
	claims.mu.Unlock()
	c.Now = func() time.Time { return t0.Add(61 * time.Second) }
	c.Tick(t.Context())

	if podGone(t, cs, "pod-a") {
		t.Error("activation with fresh traffic must not be torn down")
	}
}

func TestIdleTeardown_ZeroIdleTimeoutMeansUntilDelete(t *testing.T) {
	t.Parallel()
	o, cs, claims := newTestOrchestrator(t, testPool("web"))
	addPod(t, cs, claimedPodFixture("web", "pod-a", "10.0.0.1", "site",
		pool.Activation{Command: "serve"}))
	claims.requests["10.0.0.1"] = 0

	c := idleLoop(o)
	t0 := time.Now()
	c.Now = func() time.Time { return t0 }
	c.Tick(t.Context())
	c.Now = func() time.Time { return t0.Add(24 * time.Hour) }
	c.Tick(t.Context())

	if podGone(t, cs, "pod-a") {
		t.Error("IdleTimeoutSeconds 0 must never idle out")
	}
}
