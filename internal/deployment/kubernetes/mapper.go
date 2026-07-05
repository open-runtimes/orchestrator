package kubernetes

import (
	"maps"
	"net"
	"orchestrator/internal/artifact"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	LabelManagedBy    = "managed-by"
	LabelDeploymentID = "deployment.id"
	LabelRevision     = "deployment.revision"
	ManagedByValue    = "deployments-service"

	// AnnotationHost carries the deployment's hostname on the marker ConfigMap.
	AnnotationHost = "deployment.host"

	ContainerWorker      = "worker"
	ContainerArtifactPre = "artifact-pre"
	ContainerProxy       = "proxy"

	VolumeWorkspace = "workspace"
	VolumeTmp       = "tmp"
	workspacePath   = "/workspace"

	portNameProxy = "proxy"
	portNameAdmin = "admin"
)

// objectNameFor prefixes a deployment ID or revision name into a managed
// object name: the marker ConfigMap and HTTPRoute are dep-{id}, a revision's
// Deployment and Service are dep-{revisionName}.
func objectNameFor(name string) string {
	return "dep-" + name
}

// revisionLabels are stamped on every revision-scoped object (Deployment,
// Service, pod template).
func revisionLabels(id, revision string) map[string]string {
	return map[string]string{
		LabelManagedBy:    ManagedByValue,
		LabelDeploymentID: id,
		LabelRevision:     revision,
	}
}

// buildDeployment maps a deployment.Request to the revision's immutable
// apps/v1.Deployment. The pod selector is the revision label alone, so each
// revision owns exactly its own pods.
//
// Pod template, all sharing an emptyDir workspace:
//   - initContainer "artifact-pre" (only when the request has artifacts):
//     regular init, materializes artifacts and exits before serving starts
//   - initContainer "proxy": native sidecar (restartPolicy: Always) fronting
//     the worker; its /ready probe gates pod readiness (and so EndpointSlice
//     membership)
//   - container "worker": the user workload
func buildDeployment(req *deployment.Request, cfg Config, revision string) *appsv1.Deployment {
	labels := revisionLabels(req.ID, revision)

	spec := appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{LabelRevision: revision},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       buildPodSpec(req, cfg),
		},
	}
	if req.Replicas > 0 {
		replicas := int32(req.Replicas)
		spec.Replicas = &replicas
	}
	if req.ProgressDeadlineSeconds > 0 {
		deadline := int32(req.ProgressDeadlineSeconds)
		spec.ProgressDeadlineSeconds = &deadline
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   objectNameFor(revision),
			Labels: labels,
		},
		Spec: spec,
	}
}

// buildService maps a revision to its routable Service: port 80 → the proxy
// sidecar's data port (8000). The Service is SELECTORLESS — the endpointflip
// reconciler owns its EndpointSlice (ready workload pods when warm, activator
// pods when cold/draining), which is why the target port is numeric: there
// are no pods behind a selector to resolve a named port against.
func buildService(id, revision string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   objectNameFor(revision),
			Labels: revisionLabels(id, revision),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt32(proxy.DefaultProxyPort),
			}},
		},
	}
}

func buildPodSpec(req *deployment.Request, cfg Config) corev1.PodSpec {
	autoMount := false

	var initContainers []corev1.Container
	if len(req.Artifacts) > 0 {
		initContainers = append(initContainers, artifactPreContainer(req, cfg))
	}
	initContainers = append(initContainers, proxyContainer(req, cfg))

	return corev1.PodSpec{
		ServiceAccountName:           cfg.ServiceAccount,
		AutomountServiceAccountToken: &autoMount,
		Volumes: []corev1.Volume{
			{Name: VolumeWorkspace, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: VolumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		InitContainers: initContainers,
		Containers:     []corev1.Container{workerContainer(req, cfg)},
	}
}

// artifactPreContainer materializes the request's artifacts into the shared
// workspace before the proxy and worker start. Plain init container: runs to
// completion first.
func artifactPreContainer(req *deployment.Request, cfg Config) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "JOB_ID", Value: objectNameFor(req.ID)},
		{Name: "SHARED_VOLUME_PATH", Value: workspacePath},
	}
	// MarshalArtifacts injects each artifact's "type" field, which the sidecar
	// needs to unmarshal them back into concrete types.
	if artifactsJSON, err := artifact.MarshalArtifacts(req.Artifacts); err == nil {
		env = append(env, corev1.EnvVar{Name: "ARTIFACTS_JSON", Value: string(artifactsJSON)})
	}
	return corev1.Container{
		Name:            ContainerArtifactPre,
		Image:           cfg.JobSidecarImage,
		ImagePullPolicy: corev1.PullPolicy(cfg.SidecarImagePullPolicy),
		Args:            []string{"-mode=pre"},
		Env:             env,
		VolumeMounts:    []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspacePath}},
		SecurityContext: hardenedSecurityContext(cfg),
	}
}

// proxyContainer is the deployments-sidecar: a native sidecar (init container
// with restartPolicy Always) reverse-proxying traffic to the worker. Its
// kubelet readiness probe (GET /ready on the admin port) is what admits the
// pod into the Service's EndpointSlice.
func proxyContainer(req *deployment.Request, cfg Config) corev1.Container {
	alwaysRestart := corev1.ContainerRestartPolicyAlways
	return corev1.Container{
		Name:            ContainerProxy,
		Image:           cfg.SidecarImage,
		ImagePullPolicy: corev1.PullPolicy(cfg.SidecarImagePullPolicy),
		Env:             proxyEnv(req),
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
		VolumeMounts:    []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: workspacePath}},
		SecurityContext: hardenedSecurityContext(cfg),
	}
}

// proxyEnv stamps the internal/proxy env contract into the proxy container.
func proxyEnv(req *deployment.Request) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: proxy.EnvTarget, Value: net.JoinHostPort("127.0.0.1", strconv.Itoa(req.Port))},
	}
	if req.TimeoutSeconds > 0 {
		env = append(env, corev1.EnvVar{Name: proxy.EnvTimeoutSeconds, Value: strconv.Itoa(req.TimeoutSeconds)})
	}
	if req.Concurrency > 0 {
		env = append(env, corev1.EnvVar{Name: proxy.EnvConcurrency, Value: strconv.Itoa(req.Concurrency)})
	}
	if req.Probes == nil || req.Probes.Readiness == nil {
		return env
	}
	r := req.Probes.Readiness
	if r.Path != "" {
		env = append(env, corev1.EnvVar{Name: proxy.EnvReadinessPath, Value: r.Path})
	}
	if r.PeriodMillis > 0 {
		env = append(env, corev1.EnvVar{Name: proxy.EnvReadinessPeriodMillis, Value: strconv.Itoa(r.PeriodMillis)})
	}
	if r.TimeoutMillis > 0 {
		env = append(env, corev1.EnvVar{Name: proxy.EnvReadinessTimeoutMillis, Value: strconv.Itoa(r.TimeoutMillis)})
	}
	if r.FailureThreshold > 0 {
		env = append(env, corev1.EnvVar{Name: proxy.EnvReadinessFailureThreshold, Value: strconv.Itoa(r.FailureThreshold)})
	}
	return env
}

func workerContainer(req *deployment.Request, cfg Config) corev1.Container {
	var cmd []string
	if req.Command != "" {
		cmd = []string{"/bin/sh", "-c", req.Command}
	}

	env := make([]corev1.EnvVar, 0, len(req.Environment))
	for _, k := range slices.Sorted(maps.Keys(req.Environment)) {
		env = append(env, corev1.EnvVar{Name: k, Value: req.Environment[k]})
	}

	var probes deployment.Probes
	if req.Probes != nil {
		probes = *req.Probes
	}

	return corev1.Container{
		Name:            ContainerWorker,
		Image:           req.Image,
		ImagePullPolicy: corev1.PullPolicy(cfg.WorkerImagePullPolicy),
		Command:         cmd,
		Env:             env,
		WorkingDir:      workspacePath,
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeWorkspace, MountPath: workspacePath},
			{Name: VolumeTmp, MountPath: "/tmp"},
		},
		Resources:       workerResources(req),
		LivenessProbe:   kubeletProbe(probes.Liveness, req.Port),
		StartupProbe:    kubeletProbe(probes.Startup, req.Port),
		SecurityContext: hardenedSecurityContext(cfg),
	}
}

// workerResources sets requests = limits so deployment pods are Guaranteed QoS.
func workerResources(req *deployment.Request) corev1.ResourceRequirements {
	limits := corev1.ResourceList{}
	if req.CPU > 0 {
		limits[corev1.ResourceCPU] = resource.MustParse(strconv.FormatFloat(req.CPU, 'f', 3, 64))
	}
	if req.Memory > 0 {
		limits[corev1.ResourceMemory] = resource.MustParse(strconv.Itoa(req.Memory) + "Mi")
	}
	return corev1.ResourceRequirements{Limits: limits, Requests: maps.Clone(limits)}
}

// kubeletProbe maps a deployment.Probe to a kubelet-run probe against the
// worker's port. Kubelet probes are whole-second granularity: millisecond
// fields round UP, with a 1s floor.
func kubeletProbe(p *deployment.Probe, port int) *corev1.Probe {
	if p == nil {
		return nil
	}
	target := intstr.FromInt32(int32(port))
	handler := corev1.ProbeHandler{}
	if p.Path != "" {
		handler.HTTPGet = &corev1.HTTPGetAction{Path: p.Path, Port: target}
	} else {
		handler.TCPSocket = &corev1.TCPSocketAction{Port: target}
	}
	probe := &corev1.Probe{ProbeHandler: handler}
	if p.PeriodMillis > 0 {
		probe.PeriodSeconds = ceilSeconds(p.PeriodMillis)
	}
	if p.TimeoutMillis > 0 {
		probe.TimeoutSeconds = ceilSeconds(p.TimeoutMillis)
	}
	if p.FailureThreshold > 0 {
		probe.FailureThreshold = int32(p.FailureThreshold)
	}
	return probe
}

// ceilSeconds converts milliseconds to whole seconds, rounding up (min 1s).
func ceilSeconds(millis int) int32 {
	return int32((millis + 999) / 1000)
}

// hardenedSecurityContext is the workload hardening floor (docs/design/security.md),
// applied to every container: non-root, no privilege escalation, all
// capabilities dropped, default seccomp, read-only rootfs (writes go to the
// workspace and /tmp emptyDirs).
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
