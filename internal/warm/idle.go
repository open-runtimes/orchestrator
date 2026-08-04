package warm

import (
	"context"
	"log/slog"
	"orchestrator/pkg/pool"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// IdleReaper is the end-of-life rule every warm consumer wants: with an idle
// window declared, no request-count movement across it tears the claim down.
// The window comes from the consumer's own stored spec, the teardown from its
// own delete path; the counting is the sidecar's, so it is the same for all of
// them.
//
// Its marks are leader-local — a failover restarts the clock, delaying a
// teardown by at most one window.
type IdleReaper struct {
	m        *Manager
	window   func(pod *corev1.Pod) time.Duration
	teardown func(ctx context.Context, poolID, claimID string) error
	marks    map[string]idleMark // claim id → last request-count movement
}

// idleMark remembers a claim's cumulative request count and when it last moved.
type idleMark struct {
	requests int64
	at       time.Time
}

// NewIdleReaper builds the idle rule. window reads the claim's idle window off
// its pod (0 disables teardown for that claim); teardown removes the claim.
func NewIdleReaper(m *Manager, window func(pod *corev1.Pod) time.Duration,
	teardown func(ctx context.Context, poolID, claimID string) error) *IdleReaper {
	return &IdleReaper{m: m, window: window, teardown: teardown, marks: make(map[string]idleMark)}
}

// Hooks installs the rule in a control loop.
func (r *IdleReaper) Hooks() Hooks {
	return Hooks{Reap: r.reap, Forget: r.forget}
}

func (r *IdleReaper) reap(ctx context.Context, p *pool.Pool, pod *corev1.Pod, claimID string, now time.Time) {
	window := r.window(pod)
	if window <= 0 || pod.Status.PodIP == "" {
		return
	}
	requests, err := r.m.sc.Requests(ctx, pod.Status.PodIP)
	if err != nil {
		return
	}
	mark, ok := r.marks[claimID]
	if !ok || requests != mark.requests {
		r.marks[claimID] = idleMark{requests: requests, at: now}
		return
	}
	if now.Sub(mark.at) > window {
		slog.Info("Tearing down idle claim", "kind", r.m.cfg.Naming.Kind, "claimId", claimID, "poolId", p.ID)
		if err := r.teardown(ctx, p.ID, claimID); err != nil {
			slog.Warn("Idle teardown failed", "kind", r.m.cfg.Naming.Kind, "claimId", claimID, "error", err)
		}
	}
}

func (r *IdleReaper) forget(live map[string]bool) {
	for id := range r.marks {
		if !live[id] {
			delete(r.marks, id)
		}
	}
}
