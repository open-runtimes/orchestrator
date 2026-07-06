package kubernetes

import (
	"context"
	"encoding/json"
	"log/slog"
	"orchestrator/pkg/pool"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// controller runs the leader-gated pool loops: replenishment, poison and
// orphan GC, idle teardown, and retention GC. Its memory (orphan first-seen
// times, idle last-active marks) is deliberately leader-local: a failover
// restarts those clocks, delaying a teardown by at most one window — a
// bounded cost, not a correctness issue.
type controller struct {
	o   *Orchestrator
	now func() time.Time

	orphanSince map[string]time.Time // pod name → first seen claimed-but-unlabeled
	idle        map[string]idleMark  // activation ID → last request-count movement
}

// idleMark remembers an HTTP activation's cumulative request count and when
// it last moved.
type idleMark struct {
	requests int64
	at       time.Time
}

func newController(o *Orchestrator) *controller {
	return &controller{
		o:           o,
		now:         time.Now,
		orphanSince: make(map[string]time.Time),
		idle:        make(map[string]idleMark),
	}
}

// runControl is the leader-elected control loop entrypoint: one controller
// per leadership term, ticking until the term (or process) ends.
func (o *Orchestrator) runControl(ctx context.Context) {
	c := newController(o)
	ticker := time.NewTicker(controlTick)
	defer ticker.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick reconciles every pool once, then prunes loop memory for pods and
// activations that no longer exist.
func (c *controller) tick(ctx context.Context) {
	seenPods := make(map[string]bool)
	seenActs := make(map[string]bool)
	for i := range c.o.cfg.Pools {
		c.reconcilePool(ctx, &c.o.cfg.Pools[i], seenPods, seenActs)
	}
	for name := range c.orphanSince {
		if !seenPods[name] {
			delete(c.orphanSince, name)
		}
	}
	for id := range c.idle {
		if !seenActs[id] {
			delete(c.idle, id)
		}
	}
}

// reconcilePool inspects one pool's pods — reaping claimed ones past their
// retention or idle windows, discarding poisoned and orphaned warm ones —
// and then replenishes the countable warm set up to the pool size.
func (c *controller) reconcilePool(ctx context.Context, p *pool.Pool, seenPods, seenActs map[string]bool) {
	pods, err := c.o.poolPods(ctx, p.ID)
	if err != nil {
		slog.Warn("Pool reconcile list failed", "poolId", p.ID, "error", err)
		return
	}
	warm := 0
	for i := range pods {
		pod := &pods[i]
		seenPods[pod.Name] = true
		if pod.DeletionTimestamp != nil {
			continue
		}
		if activationID := pod.Labels[LabelActivation]; activationID != "" {
			seenActs[activationID] = true
			c.reapClaimed(ctx, p, pod, activationID)
			continue
		}
		if c.countsWarm(ctx, pod) {
			warm++
		}
	}
	for n := warm; n < p.Size; n++ {
		if _, err := c.o.createWarmPod(ctx, p); err != nil {
			slog.Warn("Warm pod create failed", "poolId", p.ID, "error", err)
			return
		}
		slog.Info("Replenished warm pod", "poolId", p.ID)
	}
}

// countsWarm decides whether an unlabeled pod counts toward the pool's warm
// size. Pods not yet running count — they are on their way, and creating
// more would over-provision. Poisoned pods (sidecar reports a failed claim)
// are deleted now; claimed-but-unlabeled pods are orphans from a crash
// mid-claim, discarded after OrphanTTL — never resold.
func (c *controller) countsWarm(ctx context.Context, pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		// A warm pod's shim never exits on its own; a terminated warm pod is
		// garbage.
		_ = c.o.deletePod(ctx, pod.Name)
		return false
	case corev1.PodRunning:
		if pod.Status.PodIP == "" {
			return true
		}
	default:
		return true // still coming up
	}
	state, err := c.o.claims.State(ctx, pod.Status.PodIP)
	if err != nil {
		return true // sidecar not answering yet — still warming
	}
	switch {
	case state.Failed:
		slog.Warn("Deleting poisoned pod", "pod", pod.Name, "error", state.Error)
		_ = c.o.deletePod(ctx, pod.Name)
		return false
	case state.Claimed:
		first, ok := c.orphanSince[pod.Name]
		if !ok {
			c.orphanSince[pod.Name] = c.now()
		} else if c.now().Sub(first) > c.o.cfg.OrphanTTL {
			slog.Warn("Deleting orphaned claimed pod", "pod", pod.Name, "activationId", state.ActivationID)
			_ = c.o.deletePod(ctx, pod.Name)
		}
		return false
	}
	return true
}

// reapClaimed applies the end-of-life rules to a claimed pod. Exec: a
// terminated workload stays queryable for ActivationRetention, then the pod
// is reaped. HTTP: with IdleTimeoutSeconds set, no request-count movement
// across the window tears the activation down.
func (c *controller) reapClaimed(ctx context.Context, p *pool.Pool, pod *corev1.Pod, activationID string) {
	if !p.HTTP() {
		if t := workloadTerminated(pod); t != nil && c.now().Sub(t.FinishedAt.Time) > c.o.cfg.ActivationRetention {
			slog.Info("Reaping exec activation past retention", "activationId", activationID, "pod", pod.Name)
			_ = c.o.deletePod(ctx, pod.Name)
		}
		return
	}
	var act pool.Activation
	_ = json.Unmarshal([]byte(pod.Annotations[AnnotationActivationSpec]), &act)
	if act.IdleTimeoutSeconds <= 0 || pod.Status.PodIP == "" {
		return
	}
	requests, err := c.o.claims.Requests(ctx, pod.Status.PodIP)
	if err != nil {
		return
	}
	mark, ok := c.idle[activationID]
	if !ok || requests != mark.requests {
		c.idle[activationID] = idleMark{requests: requests, at: c.now()}
		return
	}
	if c.now().Sub(mark.at) > time.Duration(act.IdleTimeoutSeconds)*time.Second {
		slog.Info("Deactivating idle activation", "activationId", activationID, "poolId", p.ID)
		if err := c.o.Deactivate(ctx, p.ID, activationID); err != nil {
			slog.Warn("Idle deactivation failed", "activationId", activationID, "error", err)
		}
	}
}
