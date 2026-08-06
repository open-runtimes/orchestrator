package warm

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"orchestrator/internal/config"
	"orchestrator/internal/kube"
	"orchestrator/internal/pool"
	"orchestrator/internal/workload"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	VolumeWorkspace = "workspace"
	VolumeTmp       = "tmp"
	workspacePath   = config.DefaultWorkspace
	shimPath        = workspacePath + "/.pool/shim"

	portNameProxy = "proxy"
	portNameAdmin = "admin"

	// envSharedVolume is where the sidecar and shim expect the workspace.
	envSharedVolume = config.EnvSharedVolume
)

// RandHex returns n random bytes hex-encoded (2n characters, RFC-1123 safe).
func RandHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// PoolLabels are stamped on every warm pod, and are the selector consumers
// reuse for their own objects.
func (m *Manager) PoolLabels(poolID string) map[string]string {
	return map[string]string{
		LabelManagedBy:    m.cfg.Naming.ManagedBy,
		m.cfg.Naming.Pool: poolID,
	}
}

// buildPod maps a pool onto one warm pod, named {prefix}-{id}-{suffix} (the
// name is chosen by the caller — the claim token derives from it). All
// containers share an emptyDir workspace:
//
//   - initContainer "shim-install": copies the pool-shim binary into the
//     workspace (the pool image is the user's runtime and has no shim), exits;
//   - initContainer "agent-install" (when the consumer declares an Agent): the
//     publishing image, its command replaced by a copy of the binary into the
//     workspace — how a sandbox pool serves the contract from any image;
//   - initContainer "proxy": the workload-sidecar as a native sidecar
//     (restartPolicy: Always) in pool mode — armed with the claim token, its
//     /ready probe is the warm-ready gate before a claim and the serving
//     gate after;
//   - container "workload": the pool image with its entrypoint overridden to
//     the shim, which idles on a FIFO until the sidecar signals the exec.
//
// RestartPolicy Never: when the exec'd workload exits, the pod completes and
// the kubelet SIGTERMs the sidecar — the pod is discarded, never reused.
func (m *Manager) buildPod(p *pool.Pool, name, token string) *corev1.Pod {
	autoMount := false
	podVolumes, _ := kube.PersistentVolumes(p.Volumes)
	cfg := m.cfg
	warmPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: m.PoolLabels(p.ID),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &autoMount,
			Tolerations:                  cfg.Tolerations,
			NodeSelector:                 cfg.NodeSelector,
			Volumes: append([]corev1.Volume{
				{Name: VolumeWorkspace, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: VolumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			}, podVolumes...),
			InitContainers: initContainers(p, cfg, token),
			Containers:     []corev1.Container{workloadContainer(p, cfg)},
		},
	}
	// Isolation tier (docs/operations.md): a POOL dimension — warm pods are
	// runtime-fixed at creation, so warm pools are keyed by (image,
	// runtimeClass). gvisor/kata stamp their mapped RuntimeClass; runc (the
	// default) stamps nothing. NOTE: replenishment only tops counts up — it
	// does not replace existing warm pods on config drift, so a tier change
	// applies to newly created pods only.
	if rc := kube.RuntimeClassFor(cfg.RuntimeClasses, p.RuntimeClass); rc != "" {
		warmPod.Spec.RuntimeClassName = &rc
	}
	return warmPod
}

// initContainers builds the pod's init containers in order: the shim install,
// the optional agent install, then the sidecar (a native sidecar, which the
// kubelet starts and leaves running).
func initContainers(p *pool.Pool, cfg Config, token string) []corev1.Container {
	containers := []corev1.Container{shimInstallContainer(cfg)}
	if cfg.Agent.Image != "" {
		containers = append(containers, agentInstallContainer(cfg))
	}
	return append(containers, proxyContainer(p, cfg, token))
}

// agentInstallContainer copies the contract-serving binary out of the image that
// publishes it. Plain `cp` with no shell and no mkdir, so the only thing the
// publishing image must provide is the binary and a cp — and the pinned tag or
// digest of that image IS the version pin.
func agentInstallContainer(cfg Config) corev1.Container {
	return corev1.Container{
		Name:            ContainerAgentInstall,
		Image:           cfg.Agent.Image,
		ImagePullPolicy: corev1.PullPolicy(cfg.WorkerImagePullPolicy),
		Command:         []string{"cp", cfg.Agent.Source, cfg.Agent.Dest},
		VolumeMounts:    []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspacePath}},
		SecurityContext: kube.HardenedSecurityContext(cfg.RunAsUser),
	}
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
		SecurityContext: kube.HardenedSecurityContext(cfg.RunAsUser),
	}
}

// proxyContainer is the workload-sidecar in pool mode: a native sidecar
// listening from pod start, holding the claim endpoints. Its kubelet /ready
// probe gates the pod's Ready condition — warm-ready while unclaimed.
func proxyContainer(p *pool.Pool, cfg Config, token string) corev1.Container {
	alwaysRestart := corev1.ContainerRestartPolicyAlways
	env := []corev1.EnvVar{
		{Name: workload.EnvClaimToken, Value: token},
		{Name: envSharedVolume, Value: workspacePath},
		{Name: workload.EnvTargetHost, Value: "127.0.0.1"},
	}
	if p.Mounts {
		env = append(env, corev1.EnvVar{Name: workload.EnvMounts, Value: "true"})
	}
	// The proxy materializes s3:// artifacts in-process on claim, so it needs
	// the deployments/pools service's S3 credentials.
	for _, kv := range config.LoadS3Credentials().ToEnv() {
		env = append(env, corev1.EnvVar{Name: kv[0], Value: kv[1]})
	}
	return corev1.Container{
		Name:            ContainerProxy,
		Image:           cfg.SidecarImage,
		ImagePullPolicy: corev1.PullPolicy(cfg.SidecarImagePullPolicy),
		Env:             env,
		Ports: []corev1.ContainerPort{
			{Name: portNameProxy, ContainerPort: workload.DefaultProxyPort},
			{Name: portNameAdmin, ContainerPort: workload.DefaultAdminPort},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/ready",
					Port: intstr.FromInt32(workload.DefaultAdminPort),
				},
			},
			PeriodSeconds:    1,
			FailureThreshold: 3,
		},
		RestartPolicy:   &alwaysRestart,
		Resources:       kube.SidecarResources(),
		VolumeMounts:    []corev1.VolumeMount{workspaceMount(p, corev1.MountPropagationBidirectional)},
		SecurityContext: sidecarSecurityContext(p, cfg),
	}
}

// sidecarSecurityContext hardens the sidecar, unless the pool lets a claim
// mount — then it needs privilege, which is the cost of the capability.
func sidecarSecurityContext(p *pool.Pool, cfg Config) *corev1.SecurityContext {
	if p.Mounts {
		return kube.MountingSecurityContext()
	}
	return kube.HardenedSecurityContext(cfg.RunAsUser)
}

// workspaceMount mounts the shared workspace, carrying propagation only for a
// pool that mounts: a mount established in the sidecar reaches the workload
// through the shared subtree, and the workload's copy must accept it. Nothing
// is propagated for a pool without the capability.
func workspaceMount(p *pool.Pool, propagation corev1.MountPropagationMode) corev1.VolumeMount {
	m := corev1.VolumeMount{Name: VolumeWorkspace, MountPath: workspacePath}
	if p.Mounts {
		m.MountPropagation = &propagation
	}
	return m
}

// workloadContainer is the pool image, entrypoint overridden to the installed
// shim so the container idles until a claim execs the real payload.
func workloadContainer(p *pool.Pool, cfg Config) corev1.Container {
	// The consumer's settings come first so the pool can override them: an
	// operator who sets SANDBOX_PORT explicitly means it.
	declared := map[string]string{}
	if cfg.WorkloadEnv != nil {
		maps.Copy(declared, cfg.WorkloadEnv(p))
	}
	maps.Copy(declared, p.Environment)

	env := make([]corev1.EnvVar, 0, len(declared))
	for _, k := range slices.Sorted(maps.Keys(declared)) {
		env = append(env, corev1.EnvVar{Name: k, Value: declared[k]})
	}

	_, workerVolumeMounts := kube.PersistentVolumes(p.Volumes)
	return corev1.Container{
		Name:            ContainerWorkload,
		Image:           p.Image,
		ImagePullPolicy: corev1.PullPolicy(cfg.WorkerImagePullPolicy),
		Command:         []string{shimPath},
		Env:             env,
		WorkingDir:      workspacePath,
		VolumeMounts: append([]corev1.VolumeMount{
			workspaceMount(p, corev1.MountPropagationHostToContainer),
			{Name: VolumeTmp, MountPath: "/tmp"},
		}, workerVolumeMounts...),
		Resources:       cfg.Overcommit.WorkerResources(p.CPU, p.Memory),
		SecurityContext: kube.HardenedSecurityContext(cfg.RunAsUser),
	}
}
