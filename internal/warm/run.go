package warm

import (
	"context"
	"fmt"
	"orchestrator/internal/kube"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Teardown removes one claim. Consumers supply their own — a sandbox teardown
// also removes its routing state, and the idle rule calls
// it when a claim's window passes.
type Teardown func(ctx context.Context, poolID, claimID string) error

// RunClaims starts only the leader-elected claimed-workload lifecycle loop.
// Consumer services use this after bare-pod inventory moved to pool-controller.
func (m *Manager) RunClaims(ctx context.Context, teardown Teardown) error {
	if err := m.Verify(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.stop = cancel
	hooks := NewIdleReaper(m, teardown).Hooks()
	go kube.RunLeaderElected(runCtx, m.client, m.cfg.Namespace, m.cfg.LeaderElection,
		func(loopCtx context.Context) { m.RunClaimControl(loopCtx, hooks) }, m.onLeadership)
	return nil
}

// Stop halts the control loop. Warm and claimed pods are NOT touched —
// Kubernetes keeps them independently and a restart reconciles.
func (m *Manager) Stop() {
	if m.stop != nil {
		m.stop()
	}
}

// onLeadership records leadership transitions when metrics are wired.
func (m *Manager) onLeadership(ctx context.Context, identity string, leading bool) {
	if m.cfg.Metrics != nil {
		m.cfg.Metrics.RecordLeadership(ctx, identity, leading)
	}
}

// Await blocks until the claimed pod's sidecar reports its workload serving, so
// a create can answer with an address that is already live. It returns "" once
// the workload answers, or the reason it never did — the pod is deleted first,
// so an unserving claim never keeps a pool slot, leaving the caller only
// whatever else it published to remove.
//
// The reason is only returned when that delete SUCCEEDED. A delete that failed
// comes back as an error instead, because the reason is what the caller reports
// as a failed workload — and a failed workload whose pod is still running is the
// one thing nothing downstream can fix: the pod is claimed, so both sweeps skip
// it (a claimed pod is normally live), and the caller, having been told this
// failed, will never delete it.
func (m *Manager) Await(ctx context.Context, pod *corev1.Pod) (string, error) {
	deadline := time.Now().Add(m.cfg.ServeWait)
	for !m.sc.Ready(ctx, pod.Status.PodIP) {
		if time.Now().After(deadline) {
			if err := m.DiscardErr(ctx, pod.Name); err != nil {
				return "", err
			}
			return fmt.Sprintf("workload not serving within %s", m.cfg.ServeWait), nil
		}
		if err := m.sleep(ctx); err != nil {
			return "", err
		}
	}
	return "", nil
}

// Serving reports whether a claimed pod's sidecar answers /ready right now:
// one probe, no wait, for consumers that poll from a reconcile loop rather
// than block a request on Await.
func (m *Manager) Serving(ctx context.Context, pod *corev1.Pod) bool {
	if pod.Status.PodIP == "" {
		return false
	}
	if !m.reservationAccepted(ctx, pod) {
		return false
	}
	return m.sc.Ready(ctx, pod.Status.PodIP)
}
