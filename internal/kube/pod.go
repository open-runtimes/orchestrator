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
// Applied today by the deployments backend (revision replicas), by internal/warm
// (warm pods for pools and sandboxes) and by the jobs backend's worker.

// HardenedSecurityContext is the container security context for anything we run
// ourselves: no root, no new privileges, no capabilities, no writable root
// filesystem. uid is both the user and the group. Seccomp comes from the pod
// (PodSecurity).
//
// It is deliberately NOT applied to a user's own workload container, which may
// legitimately need to be root or to write to its image's filesystem.
func HardenedSecurityContext(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             new(true),
		RunAsUser:                new(uid),
		RunAsGroup:               new(uid),
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		ReadOnlyRootFilesystem:   new(true),
	}
}

// WorkloadSecurityContext is for a container running the user's own command,
// such as a build: it keeps the image's user and a writable filesystem, since
// installers run as root and write everywhere, but takes no new privileges and
// only the capabilities a root package install uses. Seccomp comes from the pod.
//
// apk, apt and dpkg chown and chmod what they unpack (CHOWN, FOWNER, FSETID),
// installers write into files the sidecar owns (DAC_OVERRIDE), apt drops to
// _apt and scripts step down with su (SETUID, SETGID), and a build can stop
// processes it started under another user (KILL). apk and apt work without the
// rest of the runtime's defaults (NET_RAW, NET_BIND_SERVICE, MKNOD, SYS_CHROOT,
// AUDIT_WRITE, SETPCAP, SETFCAP); without SETFCAP a packaged file capability is
// only a warning.
func WorkloadSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETUID", "SETGID", "KILL"},
		},
	}
}

// MountingSecurityContext is what a container needs to loop-mount a filesystem
// image: CAP_SYS_ADMIN via privileged, and root to open the loop device. It is
// the opposite of HardenedSecurityContext and is granted to exactly one
// container — the sidecar performing the mount — and only in pods whose
// workload asked for one.
func MountingSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{Privileged: new(true), RunAsUser: new(int64(0))}
}

// FileAccessSecurityContext lets a container read and write files of any owner
// or mode: root with only CAP_DAC_OVERRIDE, since added capabilities have no
// effect for a non-root user.
func FileAccessSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                new(int64(0)),
		AllowPrivilegeEscalation: new(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"DAC_OVERRIDE"},
		},
	}
}

// PodSecurity is a workload pod's security context: the operator's
// (KUBE_WORKLOAD_POD_SECURITY_CONTEXT), with RuntimeDefault seccomp unless it
// names a profile. Containers set no seccomp, so the pod's applies to all.
func PodSecurity(operator corev1.PodSecurityContext) *corev1.PodSecurityContext {
	if operator.SeccompProfile == nil {
		operator.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	return &operator
}

// sidecarMemoryLimit caps a workload sidecar's memory, page cache included.
const sidecarMemoryLimit = "512Mi"

// SidecarResources is what a workload sidecar asks for: a small CPU request and
// a memory request, with a memory cap. No CPU limit — throttling a proxy adds
// latency to every request through it, and the request already buys its share.
//
// The cap is well above what the proxy itself uses because the sidecar also
// materializes artifacts. The page cache of every file it writes into the
// workspace is charged to its cgroup, so a download or unpack larger than the
// cap stalls the sidecar in reclaim (failing its readiness probe and dropping
// the claim in flight) rather than finishing. The memory request stays small:
// the cap is headroom for that burst, not a reservation. The CPU request is
// what the sidecar is guaranteed under contention while it downloads and
// unpacks on a busy node; with no limit it still bursts above it.
func SidecarResources() corev1.ResourceRequirements {
	requests := corev1.ResourceList{}
	requests[corev1.ResourceCPU] = resource.MustParse("100m")
	requests[corev1.ResourceMemory] = resource.MustParse("32Mi")
	limits := corev1.ResourceList{}
	limits[corev1.ResourceMemory] = resource.MustParse(sidecarMemoryLimit)
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
