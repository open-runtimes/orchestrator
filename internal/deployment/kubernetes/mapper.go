package kubernetes

import (
	"maps"
	"net"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/deployment"
	"orchestrator/internal/kube"
	revisionapi "orchestrator/internal/revision"
	"orchestrator/internal/startup"
	"orchestrator/internal/workload"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	LabelManagedBy    = "managed-by"
	LabelDeploymentID = "deployment.id"
	LabelRevision     = "deployment.revision"
	LabelReplicaSlot  = "deployment.replica-slot"
	LabelPoolClaim    = "deployment.pool-claim"
	LabelServing      = "deployment.serving"
	ManagedByValue    = "deployments-service"

	// AnnotationHost carries the deployment's hostname on the marker ConfigMap.
	AnnotationHost = "deployment.host"
	// AnnotationRevisionGeneration records the Revision spec generation a
	// replica Pod was built for, so a reconciler working from a stale cache
	// can tell the Pod is newer than its view and leave it alone.
	AnnotationRevisionGeneration = "deployment.revision-generation"

	ContainerWorker = "worker"
	ContainerProxy  = "proxy"

	VolumeWorkspace = "workspace"
	VolumeTmp       = "tmp"
	// workspacePath is the default shared-volume mount path when a request
	// does not set req.Workspace.
	workspacePath = config.DefaultWorkspace

	portNameProxy = "proxy"
	portNameAdmin = "admin"
)

// buildRevision maps a domain revision to the immutable Revision CR consumed
// by the direct-pod controller.
func buildRevision(req *deployment.Request, cfg Config, revision string) *revisionapi.Revision {
	labels := revisionLabels(req.ID, revision)
	return &revisionapi.Revision{
		TypeMeta: metav1.TypeMeta{APIVersion: revisionapi.APIVersion(), Kind: revisionapi.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectNameFor(revision),
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: revisionSpec(req, cfg, revision, labels),
	}
}

func revisionSpec(req *deployment.Request, cfg Config, revision string, labels map[string]string) revisionapi.Spec {
	timeout := req.TimeoutSeconds
	spec := revisionapi.Spec{
		Replicas:            max(int32(req.Replicas), 0),
		ReadyTimeoutSeconds: int32(req.ReadyTimeoutSeconds),
		Template: &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       buildPodSpec(req, cfg, revision),
		},
		Claim: &workload.ClaimRequest{
			ClaimID: revision,
			Command: req.Command, Environment: req.Environment, Artifacts: req.Artifacts,
			Port: req.Port, TimeoutSeconds: &timeout, Concurrency: req.Concurrency,
		},
	}
	if req.Probes != nil && req.Probes.Readiness != nil {
		probe := req.Probes.Readiness
		spec.Claim.ReadinessPath = probe.Path
		spec.Claim.ReadinessPeriodMillis = probe.PeriodMillis
		spec.Claim.ReadinessTimeoutMillis = probe.TimeoutMillis
		spec.Claim.ReadinessFailureThreshold = probe.FailureThreshold
	}
	return spec
}

// workspaceOf is the request's workspace (working directory and shared-volume
// mount path), falling back to the default for specs stored before the field
// existed. Every container in the pod must agree on it.
func workspaceOf(req *deployment.Request) string {
	if req.Workspace != "" {
		return req.Workspace
	}
	return workspacePath
}

// objectNameFor prefixes a deployment ID or revision name into a managed
// object name: the marker ConfigMap and HTTPRoute are dep-{id}; a Revision and
// its Service are dep-{revisionName}.
func objectNameFor(name string) string {
	return "dep-" + name
}

// revisionLabels are stamped on every revision-scoped object (Revision,
// Service, pod template).
func revisionLabels(id, revision string) map[string]string {
	return map[string]string{
		LabelManagedBy:    ManagedByValue,
		LabelDeploymentID: id,
		LabelRevision:     revision,
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
				TargetPort: intstr.FromInt32(workload.DefaultProxyPort),
			}},
		},
	}
}

func buildPodSpec(req *deployment.Request, cfg Config, revision string) corev1.PodSpec {
	autoMount := false

	initContainers := []corev1.Container{proxyContainer(req, cfg)}

	podVolumes, _ := kube.PersistentVolumes(req.Volumes)

	spec := corev1.PodSpec{
		ServiceAccountName:           cfg.ServiceAccount,
		AutomountServiceAccountToken: &autoMount,
		Tolerations:                  cfg.Tolerations,
		NodeSelector:                 cfg.NodeSelector,
		Volumes: append([]corev1.Volume{
			{Name: VolumeWorkspace, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: VolumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}, podVolumes...),
		InitContainers: initContainers,
		Containers:     []corev1.Container{workerContainer(req, cfg)},
		// Pack across deployments, spread within one (resource-model.md): a
		// soft per-node spread over the revision's own replicas so a node loss
		// can't take all of them, without fighting bin-packing for everyone else.
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{LabelRevision: revision},
			},
		}},
	}
	if req.TerminationGracePeriodSeconds > 0 {
		grace := int64(req.TerminationGracePeriodSeconds)
		spec.TerminationGracePeriodSeconds = &grace
	}
	// Isolation tier (docs/operations.md): gvisor/kata stamp their mapped
	// RuntimeClass; runc (the default) stamps nothing.
	if rc := kube.RuntimeClassFor(cfg.RuntimeClasses, req.RuntimeClass); rc != "" {
		spec.RuntimeClassName = &rc
	}
	return spec
}

// proxyContainer is the workload-sidecar: a native sidecar (init container
// with restartPolicy Always) reverse-proxying traffic to the worker. Its
// kubelet readiness probe (GET /ready on the admin port) is what admits the
// pod into the Service's EndpointSlice.
func proxyContainer(req *deployment.Request, cfg Config) corev1.Container {
	alwaysRestart := corev1.ContainerRestartPolicyAlways
	workspace := workspaceOf(req)
	return corev1.Container{
		Name:            ContainerProxy,
		Image:           cfg.SidecarImage,
		ImagePullPolicy: corev1.PullPolicy(cfg.SidecarImagePullPolicy),
		Env:             proxyEnv(req),
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
		VolumeMounts:    []corev1.VolumeMount{workspaceMount(req, workspace, corev1.MountPropagationBidirectional)},
		SecurityContext: sidecarSecurityContext(req, cfg),
	}
}

// sidecarSecurityContext hardens the sidecar, unless this revision mounts — then
// it needs CAP_SYS_ADMIN and root, which is the cost of the capability and the
// reason it is per-request rather than always on.
func sidecarSecurityContext(req *deployment.Request, cfg Config) *corev1.SecurityContext {
	if artifact.HasMount(req.Artifacts) {
		return kube.MountingSecurityContext()
	}
	return kube.HardenedSecurityContext(cfg.RunAsUser)
}

// workspaceMount carries propagation only for a revision that mounts: the mount
// is made in the sidecar and read in the worker, which takes Bidirectional out
// of one and HostToContainer into the other.
func workspaceMount(req *deployment.Request, workspace string, propagation corev1.MountPropagationMode) corev1.VolumeMount {
	m := corev1.VolumeMount{Name: VolumeWorkspace, MountPath: workspace}
	if artifact.HasMount(req.Artifacts) {
		m.MountPropagation = &propagation
	}
	return m
}

// proxyEnv stamps the internal/proxy env contract into the proxy container.
func proxyEnv(req *deployment.Request) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: workload.EnvTarget, Value: net.JoinHostPort("127.0.0.1", strconv.Itoa(req.Port))},
	}
	if req.TimeoutSeconds > 0 {
		env = append(env, corev1.EnvVar{Name: workload.EnvTimeoutSeconds, Value: strconv.Itoa(req.TimeoutSeconds)})
	}
	if req.Concurrency > 0 {
		env = append(env, corev1.EnvVar{Name: workload.EnvConcurrency, Value: strconv.Itoa(req.Concurrency)})
	}
	// Before the readiness early-return below: a revision that mounts must be
	// told so whether or not it configures probes, or its sidecar never mounts,
	// its worker gate never passes, and the workload never starts.
	if artifact.HasMount(req.Artifacts) {
		env = append(env, corev1.EnvVar{Name: workload.EnvMounts, Value: "true"})
	}

	env = append(env, corev1.EnvVar{Name: startup.EnvPrepare, Value: "true"}, corev1.EnvVar{Name: config.EnvSharedVolume, Value: workspaceOf(req)})
	artifactsJSON, _ := artifact.MarshalArtifacts(req.Artifacts)
	env = append(env, corev1.EnvVar{Name: workload.EnvArtifacts, Value: string(artifactsJSON)})
	for _, kv := range config.LoadS3Credentials().ToEnv() {
		env = append(env, corev1.EnvVar{Name: kv[0], Value: kv[1]})
	}

	if req.Probes == nil || req.Probes.Readiness == nil {
		return env
	}
	r := req.Probes.Readiness
	if r.Path != "" {
		env = append(env, corev1.EnvVar{Name: workload.EnvReadinessPath, Value: r.Path})
	}
	if r.PeriodMillis > 0 {
		env = append(env, corev1.EnvVar{Name: workload.EnvReadinessPeriodMillis, Value: strconv.Itoa(r.PeriodMillis)})
	}
	if r.TimeoutMillis > 0 {
		env = append(env, corev1.EnvVar{Name: workload.EnvReadinessTimeoutMillis, Value: strconv.Itoa(r.TimeoutMillis)})
	}
	if r.FailureThreshold > 0 {
		env = append(env, corev1.EnvVar{Name: workload.EnvReadinessFailureThreshold, Value: strconv.Itoa(r.FailureThreshold)})
	}
	return env
}

func workerContainer(req *deployment.Request, cfg Config) corev1.Container {
	workspace := workspaceOf(req)
	_, workerVolumeMounts := kube.PersistentVolumes(req.Volumes)

	env := make([]corev1.EnvVar, 0, len(req.Environment))
	for _, k := range slices.Sorted(maps.Keys(req.Environment)) {
		env = append(env, corev1.EnvVar{Name: k, Value: req.Environment[k]})
	}

	var probes deployment.Probes
	if req.Probes != nil {
		probes = *req.Probes
	}

	startProbe := kubeletProbe(probes.Startup, req.Port)
	if startProbe == nil {
		startProbe = &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "test -s " + startup.ExecutionMarker}}},
			PeriodSeconds: 1, FailureThreshold: int32(startup.TimeoutSeconds),
		}
	}
	return corev1.Container{
		Name:                   ContainerWorker,
		Image:                  req.Image,
		ImagePullPolicy:        corev1.PullPolicy(cfg.WorkerImagePullPolicy),
		Command:                startup.Command(req.Command, workspace, startup.TimeoutSeconds),
		TerminationMessagePath: startup.ExecutionMarker,
		Env:                    env,
		WorkingDir:             workspace,
		VolumeMounts: append([]corev1.VolumeMount{
			workspaceMount(req, workspace, corev1.MountPropagationHostToContainer),
			{Name: VolumeTmp, MountPath: "/tmp"},
		}, workerVolumeMounts...),
		Resources:       cfg.Overcommit.WorkerResources(req.CPU, req.Memory),
		LivenessProbe:   kubeletProbe(probes.Liveness, req.Port),
		StartupProbe:    startProbe,
		SecurityContext: kube.HardenedSecurityContext(cfg.RunAsUser),
	}
}

// buildPDB returns the revision's PodDisruptionBudget, or nil when the
// deployment is not durably multi-replica (fixed 1 replica, or autoscaling
// with minReplicas < 2). At one replica a minAvailable:1 PDB deadlocks node
// drains; the activator's buffering already turns eviction into a latency
// event, not downtime — the design's default posture (resource-model.md).
func buildPDB(req *deployment.Request, revision string) *policyv1.PodDisruptionBudget {
	durablyMultiReplica := req.Autoscaling == nil && req.Replicas > 1 ||
		req.Autoscaling != nil && req.Autoscaling.MinReplicas >= 2
	if !durablyMultiReplica {
		return nil
	}
	minAvailable := intstr.FromInt32(1)
	alwaysAllow := policyv1.AlwaysAllow
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:   objectNameFor(revision),
			Labels: revisionLabels(req.ID, revision),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{LabelRevision: revision},
			},
			// Broken pods never deadlock a drain.
			UnhealthyPodEvictionPolicy: &alwaysAllow,
		},
	}
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
