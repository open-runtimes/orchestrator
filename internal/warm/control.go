package warm

import (
	"context"
	"log/slog"
	"orchestrator/internal/pool"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Hooks is the consumer's part of the control loop. Both are optional.
type Hooks struct {
	// Reap applies the consumer's end-of-life rule to one claimed pod (idle
	// teardown). Called once per claimed pod per tick, with the tick's clock —
	// so a consumer needs no clock of its own, and tests drive both from one.
	Reap func(ctx context.Context, p *pool.Pool, pod *corev1.Pod, claimID string, now time.Time)
	// Forget drops the consumer's per-claim memory for claims that no longer
	// exist; live holds the claim ids seen this tick.
	Forget func(live map[string]bool)
}

// Controller runs the leader-gated loops: replenishment, poison and orphan GC,
// and the consumer's reaping. Its memory (orphan first-seen times) is
// deliberately leader-local: a failover restarts those clocks, delaying a
// teardown by at most one window — a bounded cost, not a correctness issue.
// One Controller per leadership term. Tests drive Tick directly, with Now
// replaced.
type Controller struct {
	m     *Manager
	hooks Hooks

	// Now is the loop's clock, replaced by tests.
	Now func() time.Time

	orphanSince map[string]time.Time // pod name → first seen claim-state mismatch
}

// Controller builds one control loop over these hooks.
func (m *Manager) Controller(hooks Hooks) *Controller {
	return &Controller{
		m: m, hooks: hooks, Now: time.Now,
		orphanSince: make(map[string]time.Time),
	}
}

// runControlLoop is the leader-elected control loop entrypoint: one Controller per
// leadership term, ticking until the term (or process) ends.
func (m *Manager) runControlLoop(ctx context.Context, hooks Hooks) {
	c := m.Controller(hooks)
	ticker := time.NewTicker(controlTick)
	defer ticker.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunControl runs inventory reconciliation for an already leader-gated
// caller. It avoids starting a second Lease elector inside a control plane
// that already owns a leadership term.
func (m *Manager) RunControl(ctx context.Context, hooks Hooks) { m.runControlLoop(ctx, hooks) }

// RunClaimControl runs only consumer lifecycle hooks over claimed pods. It
// deliberately does no warm inventory work: the standalone pool-controller
// owns replenishment and unclaimed-pod garbage collection.
func (m *Manager) RunClaimControl(ctx context.Context, hooks Hooks) {
	c := m.Controller(hooks)
	ticker := time.NewTicker(controlTick)
	defer ticker.Stop()
	for {
		c.TickClaims(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// TickClaims applies consumer lifecycle hooks to every claimed pod once.
func (c *Controller) TickClaims(ctx context.Context) {
	pods, err := c.m.Claimed(ctx, "", "")
	if err != nil {
		slog.Warn("Claim lifecycle list failed", "error", err)
		return
	}
	live := make(map[string]bool, len(pods))
	seenPods := make(map[string]bool, len(pods))
	now := c.Now()
	for i := range pods {
		pod := &pods[i]
		seenPods[pod.Name] = true
		if pod.DeletionTimestamp != nil {
			continue
		}
		claimID := c.m.ClaimID(pod)
		if claimID == "" {
			continue
		}
		if c.reservationPending(ctx, pod, claimID, now) {
			continue
		}
		live[claimID] = true
		if c.hooks.Reap == nil {
			continue
		}
		p := c.m.byID[c.m.PoolID(pod)]
		if p == nil {
			p = &pool.Pool{ID: c.m.PoolID(pod)}
		}
		c.hooks.Reap(ctx, p, pod, claimID, now)
	}
	c.forgetOrphans(seenPods)
	if c.hooks.Forget != nil {
		c.hooks.Forget(live)
	}
}

// Tick reconciles every pool once, then prunes loop memory for pods and claims
// that no longer exist.
func (c *Controller) Tick(ctx context.Context) {
	seenPods := make(map[string]bool)
	seenClaims := make(map[string]bool)
	for i := range c.m.pools {
		c.reconcile(ctx, &c.m.pools[i], seenPods, seenClaims)
	}
	if c.m.cfg.ReapUnpooled {
		c.reapUnpooled(ctx, seenPods, seenClaims)
	}
	// Unclaimed pods whose pool disappeared are never live workloads. Sweep
	// them for every consumer; ReapUnpooled gates only claimed workloads.
	c.sweepUnclaimed(ctx)
	c.forgetOrphans(seenPods)
	if c.hooks.Forget != nil {
		c.hooks.Forget(seenClaims)
	}
}

// reapUnpooled applies the end-of-life rule to claims whose pool is not in the
// config — a workload that brought its own pool of one, so no declared pool's
// reconcile will ever see it. Without this an abandoned one would hold its pod
// forever, which is exactly what an idle timeout exists to prevent.
//
// It only reaps. There is nothing to replenish (a pool of one is spent) and
// nothing to count.
func (c *Controller) reapUnpooled(ctx context.Context, seenPods, seenClaims map[string]bool) {
	if c.hooks.Reap == nil {
		return
	}
	pods, err := c.m.Claimed(ctx, "", "")
	if err != nil {
		slog.Warn("Unpooled reconcile list failed", "error", err)
		return
	}
	now := c.Now()
	for i := range pods {
		pod := &pods[i]
		poolID := c.m.PoolID(pod)
		if _, declared := c.m.byID[poolID]; declared {
			continue // a configured pool's reconcile already handled it
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		claimID := c.m.ClaimID(pod)
		if claimID == "" {
			continue
		}
		seenPods[pod.Name] = true
		seenClaims[claimID] = true
		if c.reservationPending(ctx, pod, claimID, now) {
			continue
		}
		c.hooks.Reap(ctx, &pool.Pool{ID: poolID}, pod, claimID, now)
	}
}

// sweepUnclaimed discards pods that were created for one request and never
// labeled with its claim. Nothing else can see them: no configured pool's
// reconcile selects a request-keyed pool id, and reapUnpooled lists only pods
// that carry a claim — so a create that died between the pod and the claim (a
// crashed replica, a failed delete) would hold capacity forever.
//
// Age decides, not loop memory, so a restart does not restart the clock. The
// threshold is ColdWait (the longest a live create can still be waiting for its
// pod) plus OrphanTTL of grace, because a pod younger than that may simply be
// mid-claim — and deleting one of those would fail a create that was going to
// succeed.
func (c *Controller) sweepUnclaimed(ctx context.Context) {
	// The pool-label existence term is essential for consumers whose direct
	// workloads share the same managed-by value. Without it, a direct Revision
	// pod has neither a pool nor claim label and looks like abandoned capacity.
	pods, err := c.m.list(ctx, c.m.managed()+","+c.m.cfg.Naming.Pool+",!"+c.m.cfg.Naming.Claim)
	if err != nil {
		slog.Warn("Unclaimed sweep list failed", "error", err)
		return
	}
	stale := c.m.cfg.ColdWait + c.m.cfg.OrphanTTL
	for i := range pods {
		pod := &pods[i]
		poolID := c.m.PoolID(pod)
		if _, declared := c.m.byID[poolID]; declared {
			continue // a configured pool's reconcile counts and GCs its own warm pods
		}
		if pod.DeletionTimestamp != nil || c.Now().Sub(pod.CreationTimestamp.Time) < stale {
			continue
		}
		slog.Warn("Discarding an unclaimed pod with no pool behind it", "pod", pod.Name, "poolId", poolID)
		if err := c.m.Delete(ctx, pod.Name); err != nil {
			slog.Warn("Failed to discard unclaimed pod", "pod", pod.Name, "error", err)
		}
	}
}

// reconcile inspects one pool's pods — handing claimed ones to the consumer's
// reaper, discarding poisoned and orphaned warm ones — then replenishes the
// countable warm set up to the pool size.
func (c *Controller) reconcile(ctx context.Context, p *pool.Pool, seenPods, seenClaims map[string]bool) {
	pods, err := c.m.Pods(ctx, p.ID)
	if err != nil {
		slog.Warn("Pool reconcile list failed", "poolId", p.ID, "error", err)
		return
	}
	warm, claimed := 0, 0
	for i := range pods {
		pod := &pods[i]
		seenPods[pod.Name] = true
		if pod.DeletionTimestamp != nil {
			continue
		}
		if claimID := c.m.ClaimID(pod); claimID != "" {
			seenClaims[claimID] = true
			claimed++
			if c.reservationPending(ctx, pod, claimID, c.Now()) {
				continue
			}
			if c.hooks.Reap != nil {
				c.hooks.Reap(ctx, p, pod, claimID, c.Now())
			}
			continue
		}
		if pod.Annotations[annotationPoolSpecHash] != poolSpecHash(&p.Spec) {
			slog.Info("Discarding stale warm pod after pool spec change", "pod", pod.Name, "poolId", p.ID)
			if err := c.m.Delete(ctx, pod.Name); err != nil {
				slog.Warn("Failed to discard stale warm pod", "pod", pod.Name, "error", err)
			}
			continue
		}
		if c.countsWarm(ctx, pod) {
			warm++
		}
	}
	if c.m.cfg.Metrics != nil {
		c.m.cfg.Metrics.RecordPoolCapacity(ctx, c.m.cfg.Naming.Kind, p.ID, int64(warm), int64(claimed))
	}
	for n := warm; n < p.Size; n++ {
		if _, err := c.m.Create(ctx, &p.Spec, p.ID); err != nil {
			slog.Warn("Warm pod create failed", "poolId", p.ID, "error", err)
			return
		}
		slog.Info("Replenished warm pod", "poolId", p.ID)
	}
}

// reservationPending protects the gap between the atomic metadata reservation
// and the sidecar claim request. A process crash in that gap leaves a fully
// identified pod whose workload never started. The orphan clock begins only
// after a successful probe observes a mismatch. Probe errors leave the pod
// alone and do not advance that clock, so a transient timeout can never delete
// a live workload after a leader failover.
func (c *Controller) reservationPending(ctx context.Context, pod *corev1.Pod, claimID string, now time.Time) bool {
	if pod.Annotations[AnnotationReservedAt] == "" {
		return false // claim created before reservation-first claiming
	}
	state, err := c.m.sc.State(ctx, pod.Status.PodIP)
	if err != nil {
		delete(c.orphanSince, pod.Name)
		return true // sidecar not answering — reservation may be live
	}
	if state.Claimed && state.ClaimID == claimID {
		delete(c.orphanSince, pod.Name)
		return false
	}
	started := c.orphanSince[pod.Name]
	if started.IsZero() {
		c.orphanSince[pod.Name] = now
		return true
	}
	if now.Sub(started) > c.m.cfg.OrphanTTL {
		slog.Warn("Deleting abandoned workload reservation", "pod", pod.Name, "claimId", claimID)
		_ = c.m.Delete(ctx, pod.Name)
	}
	return true
}

func (c *Controller) forgetOrphans(seenPods map[string]bool) {
	for name := range c.orphanSince {
		if !seenPods[name] {
			delete(c.orphanSince, name)
		}
	}
}

// countsWarm decides whether an unlabeled pod counts toward the pool's warm
// size. Pods not yet running count — they are on their way, and creating more
// would over-provision. Poisoned pods (sidecar reports a failed claim) are
// deleted now; claimed-but-unlabeled pods are orphans from a crash mid-claim,
// discarded after OrphanTTL — never resold.
func (c *Controller) countsWarm(ctx context.Context, pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		// A warm pod's shim never exits on its own; a terminated warm pod is
		// garbage.
		_ = c.m.Delete(ctx, pod.Name)
		return false
	case corev1.PodRunning:
		if pod.Status.PodIP == "" {
			return true
		}
	default:
		return true // still coming up
	}
	state, err := c.m.sc.State(ctx, pod.Status.PodIP)
	if err != nil {
		return true // sidecar not answering yet — still warming
	}
	switch {
	case state.Failed:
		slog.Warn("Deleting poisoned pod", "pod", pod.Name, "error", state.Error)
		_ = c.m.Delete(ctx, pod.Name)
		return false
	case state.Claimed:
		first, ok := c.orphanSince[pod.Name]
		if !ok {
			c.orphanSince[pod.Name] = c.Now()
		} else if c.Now().Sub(first) > c.m.cfg.OrphanTTL {
			slog.Warn("Deleting orphaned claimed pod", "pod", pod.Name, "claimId", state.ClaimID)
			_ = c.m.Delete(ctx, pod.Name)
		}
		return false
	}
	return true
}
