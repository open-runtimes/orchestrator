package kubernetes

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"math"
	"orchestrator/internal/kube"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/pool"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	LabelManagedBy  = "managed-by"
	LabelPoolID     = "pool.id"
	LabelActivation = "pool.activation"
	ManagedByValue  = "deployments-service"

	// AnnotationActivationSpec carries the accepted pool.Activation JSON on
	// claimed pods — the Status reconstruction source. The callback signing
	// key is stripped before writing (see bindPod); claim tokens are derived,
	// never annotated (see token.go).
	AnnotationActivationSpec = "pool.activation-spec"

	ContainerShimInstall = "shim-install"
	ContainerProxy       = "proxy"
	ContainerWorkload    = "workload"

	VolumeWorkspace = "workspace"
	VolumeTmp       = "tmp"
	workspacePath   = "/workspace"
	shimPath        = workspacePath + "/.pool/shim"

	portNameProxy = "proxy"
	portNameAdmin = "admin"

	// envSharedVolume is where the sidecar and shim expect the workspace.
	envSharedVolume = "SHARED_VOLUME_PATH"
)

// poolLabels are stamped on every warm pod.
func poolLabels(poolID string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelPoolID:    poolID,
	}
}

// activationLabels are stamped on an activation's Service and HTTPRoute (the
// claimed pod gains LabelActivation by patch instead).
func activationLabels(poolID, activationID string) map[string]string {
	return map[string]string{
		LabelManagedBy:  ManagedByValue,
		LabelPoolID:     poolID,
		LabelActivation: activationID,
	}
}

// randHex returns n random bytes hex-encoded (2n characters, RFC-1123 safe).
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// buildWarmPod maps a pool onto one warm pod, named pool-{id}-{suffix} (the
// name is chosen by the caller — the claim token derives from it). All
// containers share an emptyDir workspace:
//
//   - initContainer "shim-install": copies the pool-shim binary into the
//     workspace (the pool image is the user's runtime and has no shim), exits;
//   - initContainer "proxy": the deployments-sidecar as a native sidecar
//     (restartPolicy: Always) in pool mode — armed with the claim token, its
//     /ready probe is the warm-ready gate before a claim and the serving
//     gate after;
//   - container "workload": the pool image with its entrypoint overridden to
//     the shim, which idles on a FIFO until the sidecar signals the exec.
//
// RestartPolicy Never: when the exec'd workload exits, the pod completes and
// the kubelet SIGTERMs the sidecar — the pod is discarded, never reused.
func buildWarmPod(p *pool.Pool, cfg Config, name, token string) *corev1.Pod {
	autoMount := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: poolLabels(p.ID),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &autoMount,
			Volumes: []corev1.Volume{
				{Name: VolumeWorkspace, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: VolumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			InitContainers: []corev1.Container{shimInstallContainer(cfg), proxyContainer(cfg, token)},
			Containers:     []corev1.Container{workloadContainer(p, cfg)},
		},
	}
	// Sandbox tier (docs/design/security.md): a POOL dimension — warm pods
	// are runtime-fixed at creation, so warm fleets are keyed by (image,
	// sandbox). gvisor/kata stamp their mapped RuntimeClass; runc (the
	// default) stamps nothing. NOTE: replenishment only tops counts up — it
	// does not replace existing warm pods on config drift, so a sandbox
	// change applies to newly created pods only.
	if rc := kube.RuntimeClassFor(cfg.SandboxRuntimeClasses, p.Sandbox); rc != "" {
		pod.Spec.RuntimeClassName = &rc
	}
	return pod
}

// shimInstallContainer copies the shim binary into the shared workspace
// before anything else runs. Plain init container: runs to completion first.
func shimInstallContainer(cfg Config) corev1.Container {
	return corev1.Container{
		Name:            ContainerShimInstall,
		Image:           cfg.ShimImage,
		ImagePullPolicy: corev1.PullPolicy(cfg.SidecarImagePullPolicy),
		Args:            []string{"-install", shimPath},
		VolumeMounts:    []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspacePath}},
		SecurityContext: hardenedSecurityContext(cfg),
	}
}

// proxyContainer is the deployments-sidecar in pool mode: a native sidecar
// listening from pod start, holding the claim endpoints. Its kubelet /ready
// probe gates the pod's Ready condition — warm-ready while unclaimed.
func proxyContainer(cfg Config, token string) corev1.Container {
	alwaysRestart := corev1.ContainerRestartPolicyAlways
	return corev1.Container{
		Name:            ContainerProxy,
		Image:           cfg.SidecarImage,
		ImagePullPolicy: corev1.PullPolicy(cfg.SidecarImagePullPolicy),
		Env: []corev1.EnvVar{
			{Name: proxy.EnvClaimToken, Value: token},
			{Name: envSharedVolume, Value: workspacePath},
			{Name: proxy.EnvTargetHost, Value: "127.0.0.1"},
		},
		Ports: []corev1.ContainerPort{
			{Name: portNameProxy, ContainerPort: proxy.DefaultProxyPort},
			{Name: portNameAdmin, ContainerPort: proxy.DefaultAdminPort},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/ready",
					Port: intstr.FromInt32(proxy.DefaultAdminPort),
				},
			},
			PeriodSeconds:    1,
			FailureThreshold: 3,
		},
		RestartPolicy:   &alwaysRestart,
		Resources:       proxyResources(),
		VolumeMounts:    []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspacePath}},
		SecurityContext: hardenedSecurityContext(cfg),
	}
}

// proxyResources is the sidecar's small fixed overhead shape: modest
// requests, a memory ceiling, and no cpu limit (CFS-throttling avoidance,
// same rationale as the workload).
func proxyResources() corev1.ResourceRequirements {
	requests := corev1.ResourceList{}
	requests[corev1.ResourceCPU] = resource.MustParse("25m")
	requests[corev1.ResourceMemory] = resource.MustParse("32Mi")
	limits := corev1.ResourceList{}
	limits[corev1.ResourceMemory] = resource.MustParse("64Mi")
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

// workloadContainer is the pool image, entrypoint overridden to the installed
// shim so the container idles until an activation execs the real payload.
func workloadContainer(p *pool.Pool, cfg Config) corev1.Container {
	env := make([]corev1.EnvVar, 0, len(p.Environment))
	for _, k := range slices.Sorted(maps.Keys(p.Environment)) {
		env = append(env, corev1.EnvVar{Name: k, Value: p.Environment[k]})
	}
	return corev1.Container{
		Name:            ContainerWorkload,
		Image:           p.Image,
		ImagePullPolicy: corev1.PullPolicy(cfg.WorkerImagePullPolicy),
		Command:         []string{shimPath},
		Env:             env,
		WorkingDir:      workspacePath,
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeWorkspace, MountPath: workspacePath},
			{Name: VolumeTmp, MountPath: "/tmp"},
		},
		Resources:       workloadResources(p),
		SecurityContext: hardenedSecurityContext(cfg),
	}
}

// workloadResources derives the workload's resources from the pool spec:
// memory request = limit (incompressible; overcommit risks node OOM), cpu
// request only with NO cpu limit (CFS-quota throttling at a limit is a
// tail-latency killer; requests handle fairness, bursting rides idle
// headroom). Zero fields stay unset.
func workloadResources(p *pool.Pool) corev1.ResourceRequirements {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	if p.CPU > 0 {
		requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(max(int64(math.Ceil(p.CPU*1000)), 1), resource.DecimalSI)
	}
	if p.Memory > 0 {
		mem := resource.MustParse(strconv.Itoa(p.Memory) + "Mi")
		requests[corev1.ResourceMemory] = mem
		limits[corev1.ResourceMemory] = mem
	}
	return corev1.ResourceRequirements{Limits: limits, Requests: requests}
}

// hardenedSecurityContext is the workload hardening floor
// (docs/design/security.md), applied to every container: non-root, no
// privilege escalation, all capabilities dropped, default seccomp, read-only
// rootfs (writes go to the workspace and /tmp emptyDirs).
func hardenedSecurityContext(cfg Config) *corev1.SecurityContext {
	nonRoot := true
	noEscalation := false
	readOnlyRootFS := true
	uid := cfg.RunAsUser
	return &corev1.SecurityContext{
		RunAsNonRoot:             &nonRoot,
		RunAsUser:                &uid,
		RunAsGroup:               &uid,
		AllowPrivilegeEscalation: &noEscalation,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		ReadOnlyRootFilesystem:   &readOnlyRootFS,
	}
}
