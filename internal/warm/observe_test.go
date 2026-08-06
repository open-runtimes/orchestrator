package warm

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// observed builds a claimed pod in one state or another. No clientset: the
// phase rule is a function of the pod object, and this is the whole test
// surface both warm consumers derive their status from.
func observedPod(mutate func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "pool-web-aaaaa",
			Labels: map[string]string{testNaming.Pool: "web", testNaming.Claim: "act-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mutate(pod)
	return pod
}

func TestObserve_PhaseLadder(t *testing.T) {
	t.Parallel()
	m := &Manager{cfg: Config{Naming: testNaming}}

	ready := func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	exited := func(p *corev1.Pod) {
		ready(p)
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  ContainerWorkload,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
		}}
	}

	tests := []struct {
		name    string
		mutate  func(*corev1.Pod)
		want    Phase
		wantErr string
	}{
		{"claimed but not yet serving", func(*corev1.Pod) {}, PhaseStarting, ""},
		{"serving", ready, PhaseServing, ""},
		// A claimed workload has no business exiting, however clean the code.
		{"workload exit is a failure", exited, PhaseFailed, "workload exited with code 3"},
		{"pod failure carries its reason", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.Reason = "Evicted"
		}, PhaseFailed, "Evicted"},
		// Deletion wins over everything: a pod on its way out is not failed,
		// even though teardown makes its workload exit.
		{"deletion outranks the exit it causes", func(p *corev1.Pod) {
			exited(p)
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}, PhaseTerminating, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obs := m.Observe(observedPod(tt.mutate))
			if obs.Phase != tt.want {
				t.Errorf("phase: want %v, got %v", tt.want, obs.Phase)
			}
			if obs.Error != tt.wantErr {
				t.Errorf("error: want %q, got %q", tt.wantErr, obs.Error)
			}
			// Identity comes off the labels, so a reconstructed claim is
			// addressable whichever phase it is in.
			if obs.ClaimID != "act-1" || obs.PoolID != "web" || obs.PodName != "pool-web-aaaaa" {
				t.Errorf("identity: got %+v", obs)
			}
		})
	}
}
