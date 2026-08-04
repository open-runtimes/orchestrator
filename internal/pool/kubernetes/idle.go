package kubernetes

import (
	"context"
	"log/slog"
	"orchestrator/internal/warm"
	"orchestrator/pkg/pool"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// idleReaper is the activation end-of-life rule inside the warm control loop:
// with IdleTimeoutSeconds set, no request-count movement across the window
// tears the activation down. Its marks are leader-local — a failover restarts
// the clock, delaying a teardown by at most one window.
type idleReaper struct {
	o     *Orchestrator
	marks map[string]idleMark // activation ID → last request-count movement
}

// idleMark remembers an activation's cumulative request count and when it last
// moved.
type idleMark struct {
	requests int64
	at       time.Time
}

func newIdleReaper(o *Orchestrator) *idleReaper {
	return &idleReaper{o: o, marks: make(map[string]idleMark)}
}

func (r *idleReaper) hooks() warm.Hooks {
	return warm.Hooks{Reap: r.reap, Forget: r.forget}
}

func (r *idleReaper) reap(ctx context.Context, p *pool.Pool, pod *corev1.Pod, activationID string, now time.Time) {
	var act pool.Activation
	r.o.warm.Spec(pod, &act)
	if act.IdleTimeoutSeconds <= 0 || pod.Status.PodIP == "" {
		return
	}
	requests, err := r.o.warm.Sidecar().Requests(ctx, pod.Status.PodIP)
	if err != nil {
		return
	}
	mark, ok := r.marks[activationID]
	if !ok || requests != mark.requests {
		r.marks[activationID] = idleMark{requests: requests, at: now}
		return
	}
	if now.Sub(mark.at) > time.Duration(act.IdleTimeoutSeconds)*time.Second {
		slog.Info("Deactivating idle activation", "activationId", activationID, "poolId", p.ID)
		if err := r.o.Deactivate(ctx, p.ID, activationID); err != nil {
			slog.Warn("Idle deactivation failed", "activationId", activationID, "error", err)
		}
	}
}

func (r *idleReaper) forget(live map[string]bool) {
	for id := range r.marks {
		if !live[id] {
			delete(r.marks, id)
		}
	}
}
