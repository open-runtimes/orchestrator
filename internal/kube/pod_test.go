package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The policy, pinned where it lives. Every field matters and none of them fails
// loudly if it regresses: a pod with a writable root filesystem or a full
// capability set comes up exactly like a hardened one.
func TestHardenedSecurityContext(t *testing.T) {
	t.Parallel()
	sc := HardenedSecurityContext(65532)

	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("must run as non-root")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 65532 || sc.RunAsGroup == nil || *sc.RunAsGroup != 65532 {
		t.Errorf("uid/gid: got %v/%v", sc.RunAsUser, sc.RunAsGroup)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("privilege escalation must be denied")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities: got %+v, want all dropped", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccomp: got %+v", sc.SeccompProfile)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("root filesystem must be read-only")
	}
}

// A sidecar gets a memory cap and no CPU cap: throttling a proxy would add
// latency to every request through it.
func TestSidecarResources_CapsMemoryAndNotCPU(t *testing.T) {
	t.Parallel()
	r := SidecarResources()

	if _, capped := r.Limits[corev1.ResourceCPU]; capped {
		t.Error("a sidecar must not have a CPU limit")
	}
	if r.Limits.Memory().IsZero() {
		t.Error("memory must be capped")
	}
	if r.Requests.Cpu().IsZero() || r.Requests.Memory().IsZero() {
		t.Errorf("requests: got %+v", r.Requests)
	}
}

func TestPodReady(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		conditions []corev1.PodCondition
		want       bool
	}{
		{"ready", []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, true},
		{"not ready", []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}, false},
		// A pod that has not reported the condition yet is not ready — it must
		// not be routed to, and it must not be claimed.
		{"condition absent", []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}, false},
		{"no conditions", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status:     corev1.PodStatus{Conditions: tt.conditions},
			}
			if got := PodReady(pod); got != tt.want {
				t.Errorf("want %v, got %v", tt.want, got)
			}
		})
	}
}
