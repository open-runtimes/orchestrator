package warm

import (
	"cmp"
	"fmt"
	"orchestrator/internal/kube"

	corev1 "k8s.io/api/core/v1"
)

// Phase is the neutral lifecycle of a claimed pod. Consumers map it onto their
// own published vocabulary — a claim may be "starting" where a sandbox is
// "creating" — but the rule that decides which phase a pod is in lives here,
// once, for every consumer.
type Phase int

const (
	PhaseStarting    Phase = iota // claimed; artifacts materializing or the image still starting
	PhaseServing                  // the workload answers
	PhaseFailed                   // the workload exited, or the pod itself failed
	PhaseTerminating              // teardown in flight
)

// Observation is what a claimed pod says about itself, before any consumer
// vocabulary is applied.
type Observation struct {
	ClaimID string
	PoolID  string
	PodName string
	Phase   Phase
	Error   string // set when Phase is PhaseFailed
}

// Observe derives a claimed pod's phase. Deletion in flight wins over
// everything — a pod on its way out is not failed. Then a workload exit: a
// claimed workload has no business exiting, so that is a failure however clean
// the exit code. Then the pod's own failure, then readiness (the
// kubelet-probed sidecar gate).
func (m *Manager) Observe(pod *corev1.Pod) Observation {
	obs := Observation{ClaimID: m.ClaimID(pod), PoolID: m.PoolID(pod), PodName: pod.Name}
	terminated := WorkloadTerminated(pod)
	switch {
	case pod.DeletionTimestamp != nil:
		obs.Phase = PhaseTerminating
	case terminated != nil:
		obs.Phase = PhaseFailed
		obs.Error = fmt.Sprintf("workload exited with code %d", terminated.ExitCode)
	case pod.Status.Phase == corev1.PodFailed:
		obs.Phase = PhaseFailed
		obs.Error = cmp.Or(pod.Status.Message, pod.Status.Reason)
	case kube.PodReady(pod):
		obs.Phase = PhaseServing
	default:
		obs.Phase = PhaseStarting
	}
	return obs
}
