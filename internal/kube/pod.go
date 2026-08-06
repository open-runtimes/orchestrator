package kube

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The shape we give the containers we own, and how we read a pod back. Every
// backend that builds a pod spec answers with these rather than restating them:
// a hardening policy written twice is a hardening policy that drifts, and the
// drift is silent — a pod comes up either way.
//
// Applied today by the deployments backend (revision replicas) and by
// internal/warm (warm pods for pools and sandboxes). The jobs backend does not
// harden its containers; that predates this package and is a behaviour question,
// not a duplication one.

// HardenedSecurityContext is the container security context for anything we run
// ourselves: no root, no new privileges, no capabilities, no writable root
// filesystem, and the runtime's default seccomp profile. uid is both the user
// and the group.
//
// It is deliberately NOT applied to a user's own workload container, which may
// legitimately need to be root or to write to its image's filesystem.
func HardenedSecurityContext(uid int64) *corev1.SecurityContext {
	nonRoot := true
	noEscalation := false
	readOnlyRootFS := true
	user := uid
	return &corev1.SecurityContext{
		RunAsNonRoot:             &nonRoot,
		RunAsUser:                &user,
		RunAsGroup:               &user,
		AllowPrivilegeEscalation: &noEscalation,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		ReadOnlyRootFilesystem:   &readOnlyRootFS,
	}
}

// MountingSecurityContext is what a container needs to loop-mount a filesystem
// image: CAP_SYS_ADMIN via privileged, and root to open the loop device. It is
// the opposite of HardenedSecurityContext and is granted to exactly one
// container — the sidecar performing the mount — and only in pods whose
// workload asked for one.
func MountingSecurityContext() *corev1.SecurityContext {
	privileged := true
	root := int64(0)
	return &corev1.SecurityContext{Privileged: &privileged, RunAsUser: &root}
}

// SidecarResources is what a workload sidecar asks for: a small CPU request and
// a memory request with a matching cap. No CPU limit — throttling a proxy adds
// latency to every request through it, and the request already buys its share.
func SidecarResources() corev1.ResourceRequirements {
	requests := corev1.ResourceList{}
	requests[corev1.ResourceCPU] = resource.MustParse("25m")
	requests[corev1.ResourceMemory] = resource.MustParse("32Mi")
	limits := corev1.ResourceList{}
	limits[corev1.ResourceMemory] = resource.MustParse("64Mi")
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

// PodReady reports the pod's Ready condition — for our pods, the kubelet's view
// of the sidecar's readiness probe, which is the gate for both routing and
// claiming.
func PodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
