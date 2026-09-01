package kubernetes

import (
	"encoding/json"
	"orchestrator/internal/artifact"
	"orchestrator/internal/deployment"
	"orchestrator/internal/volume"
	"orchestrator/internal/workload"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
)

func testRequest() *deployment.Request {
	return &deployment.Request{
		ID:                  "web",
		Image:               "nginx:1.27",
		Command:             "nginx -g 'daemon off;'",
		CPU:                 0.5,
		Memory:              128,
		Environment:         map[string]string{"FOO": "bar", "BAZ": "qux"},
		Hosts:               []string{"web.example.com"},
		Port:                8080,
		Replicas:            2,
		Concurrency:         4,
		TimeoutSeconds:      300,
		ReadyTimeoutSeconds: 600,
	}
}

func TestBuildRevision_PoolAcquisition(t *testing.T) {
	req := testRequest()
	req.Image, req.Pool, req.Port, req.Concurrency = "", "node", 3000, 7
	req.Probes = &deployment.Probes{Readiness: &deployment.Probe{Path: "/ready", PeriodMillis: 250}}
	revision := buildRevision(req, Config{Namespace: "orchestrator"}, "web-00001")
	if revision.Spec.Template != nil || revision.Spec.Pool != "node" || revision.Spec.Claim == nil {
		t.Fatalf("pool acquisition = %+v", revision.Spec)
	}
	if revision.Spec.Claim.Port != 3000 || revision.Spec.Claim.Concurrency != 7 || revision.Spec.Claim.ReadinessPath != "/ready" {
		t.Fatalf("claim = %+v", revision.Spec.Claim)
	}
}

func mustSpecJSON(t *testing.T, req *deployment.Request) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(b)
}

func TestBuildRevision_BasicStructure(t *testing.T) {
	t.Parallel()
	req := testRequest()
	cfg := Config{SidecarImage: "sidecar:latest", ServiceAccount: "deployments", RunAsUser: 65532}

	d := buildRevision(req, cfg, "web-00001")

	if d.Name != "dep-web-00001" {
		t.Errorf("Name: want dep-web-00001, got %s", d.Name)
	}
	for _, labels := range []map[string]string{d.Labels, d.Spec.Template.Labels} {
		if labels[LabelManagedBy] != ManagedByValue || labels[LabelDeploymentID] != "web" || labels[LabelRevision] != "web-00001" {
			t.Errorf("labels: got %v", labels)
		}
	}
	if d.Spec.Replicas != 2 {
		t.Errorf("Replicas: want 2, got %v", d.Spec.Replicas)
	}
	if d.Spec.ReadyTimeoutSeconds != 600 {
		t.Errorf("ReadyTimeoutSeconds: want 600, got %v", d.Spec.ReadyTimeoutSeconds)
	}

	spec := d.Spec.Template.Spec
	if spec.ServiceAccountName != "deployments" {
		t.Errorf("ServiceAccountName: got %s", spec.ServiceAccountName)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken: want false")
	}
	if len(spec.Volumes) != 2 || spec.Volumes[0].Name != VolumeWorkspace || spec.Volumes[1].Name != VolumeTmp ||
		spec.Volumes[0].EmptyDir == nil || spec.Volumes[1].EmptyDir == nil {
		t.Errorf("Volumes: want emptyDirs workspace+tmp, got %+v", spec.Volumes)
	}

	// No artifacts → only the proxy init container.
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != ContainerProxy {
		t.Fatalf("InitContainers: want [proxy], got %+v", spec.InitContainers)
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != ContainerWorker {
		t.Fatalf("Containers: want [worker], got %+v", spec.Containers)
	}
}

func TestBuildRevision_ProxySidecar(t *testing.T) {
	t.Parallel()
	req := testRequest()
	cfg := Config{SidecarImage: "sidecar:latest", SidecarImagePullPolicy: "Never", RunAsUser: 65532}

	p := buildRevision(req, cfg, "web-00001").Spec.Template.Spec.InitContainers[0]

	if p.Image != "sidecar:latest" || p.ImagePullPolicy != corev1.PullNever {
		t.Errorf("image/pull: got %s/%s", p.Image, p.ImagePullPolicy)
	}
	if p.RestartPolicy == nil || *p.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("RestartPolicy: want Always (native sidecar), got %v", p.RestartPolicy)
	}
	if len(p.Args) != 0 {
		t.Errorf("Args: want none (dedicated workload-sidecar binary), got %v", p.Args)
	}
	if !envHas(p.Env, "PROXY_TARGET", "127.0.0.1:8080") {
		t.Errorf("env missing PROXY_TARGET=127.0.0.1:8080: %v", p.Env)
	}
	if !envHas(p.Env, "PROXY_TIMEOUT_SECONDS", "300") || !envHas(p.Env, "PROXY_CONCURRENCY", "4") {
		t.Errorf("env missing timeout/concurrency: %v", p.Env)
	}
	if len(p.Ports) != 2 || p.Ports[0].ContainerPort != 8000 || p.Ports[0].Name != "proxy" ||
		p.Ports[1].ContainerPort != 8001 || p.Ports[1].Name != "admin" {
		t.Errorf("Ports: got %+v", p.Ports)
	}
	rp := p.ReadinessProbe
	if rp == nil || rp.HTTPGet == nil || rp.HTTPGet.Path != "/ready" || rp.HTTPGet.Port.IntValue() != 8001 {
		t.Fatalf("ReadinessProbe: want GET /ready:8001, got %+v", rp)
	}
	if rp.PeriodSeconds != 1 || rp.FailureThreshold != 3 {
		t.Errorf("ReadinessProbe timing: got period=%d threshold=%d", rp.PeriodSeconds, rp.FailureThreshold)
	}
	if len(p.VolumeMounts) != 1 || p.VolumeMounts[0].Name != VolumeWorkspace || p.VolumeMounts[0].MountPath != "/workspace" {
		t.Errorf("VolumeMounts: got %+v", p.VolumeMounts)
	}
}

func TestBuildRevision_ProxyReadinessEnv(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Probes = &deployment.Probes{
		Readiness: &deployment.Probe{Path: "/healthz", PeriodMillis: 250, TimeoutMillis: 100, FailureThreshold: 5},
	}

	p := buildRevision(req, Config{}, "web-00001").Spec.Template.Spec.InitContainers[0]

	for name, want := range map[string]string{
		"PROXY_READINESS_PATH":              "/healthz",
		"PROXY_READINESS_PERIOD_MS":         "250",
		"PROXY_READINESS_TIMEOUT_MS":        "100",
		"PROXY_READINESS_FAILURE_THRESHOLD": "5",
	} {
		if !envHas(p.Env, name, want) {
			t.Errorf("env missing %s=%s: %v", name, want, p.Env)
		}
	}
}

// A custom workspace flows to the worker's WorkingDir and every workspace
// mount, so the shared-volume contract holds across containers.
func TestBuildRevision_CustomWorkspace(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Workspace = "/usr/local/server"
	cfg := Config{WorkerImagePullPolicy: "IfNotPresent"}

	w := buildRevision(req, cfg, "web-00001").Spec.Template.Spec.Containers[0]

	if w.WorkingDir != "/usr/local/server" {
		t.Errorf("WorkingDir: got %s", w.WorkingDir)
	}
	for _, m := range w.VolumeMounts {
		if m.Name == VolumeWorkspace && m.MountPath != "/usr/local/server" {
			t.Errorf("workspace mount path: got %s", m.MountPath)
		}
	}
}

func TestBuildRevision_Worker(t *testing.T) {
	t.Parallel()
	req := testRequest()
	cfg := Config{WorkerImagePullPolicy: "IfNotPresent"}

	w := buildRevision(req, cfg, "web-00001").Spec.Template.Spec.Containers[0]

	if w.Image != "nginx:1.27" || w.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("image/pull: got %s/%s", w.Image, w.ImagePullPolicy)
	}
	if !reflect.DeepEqual(w.Command, []string{"/bin/sh", "-c", "nginx -g 'daemon off;'"}) {
		t.Errorf("Command: got %v", w.Command)
	}
	if w.WorkingDir != "/workspace" {
		t.Errorf("WorkingDir: got %s", w.WorkingDir)
	}
	// Environment is emitted in sorted key order for deterministic templates.
	if len(w.Env) != 2 || w.Env[0].Name != "BAZ" || w.Env[1].Name != "FOO" || w.Env[1].Value != "bar" {
		t.Errorf("Env: got %v", w.Env)
	}
	mounts := map[string]string{}
	for _, m := range w.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts[VolumeWorkspace] != "/workspace" || mounts[VolumeTmp] != "/tmp" {
		t.Errorf("VolumeMounts: got %v", mounts)
	}

	// Burstable, memory-protected (resource-model.md): memory request ==
	// limit, cpu request derived from the limit, NO cpu limit (CFS throttling).
	if _, ok := w.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("cpu limit: want none, got %s", w.Resources.Limits.Cpu())
	}
	if cpu := w.Resources.Requests.Cpu(); cpu.MilliValue() != 500 {
		t.Errorf("cpu request: want 500m (overcommit 1), got %s", cpu)
	}
	if mem := w.Resources.Limits.Memory(); mem.Value() != 128*1024*1024 {
		t.Errorf("memory limit: want 128Mi, got %s", mem)
	}
	if !w.Resources.Requests.Memory().Equal(*w.Resources.Limits.Memory()) {
		t.Errorf("memory request != limit: %+v", w.Resources)
	}
}

func TestBuildRevision_Tolerations(t *testing.T) {
	t.Parallel()
	cfg := Config{Tolerations: []corev1.Toleration{{Key: "workload", Value: "edge-builds", Effect: corev1.TaintEffectNoSchedule}}}
	got := buildRevision(testRequest(), cfg, "web-00001").Spec.Template.Spec.Tolerations
	if len(got) != 1 || got[0].Key != "workload" {
		t.Errorf("tolerations: want workload=edge-builds:NoSchedule, got %+v", got)
	}
}

func TestBuildRevision_ProxyResources(t *testing.T) {
	t.Parallel()
	p := buildRevision(testRequest(), Config{}, "web-00001").Spec.Template.Spec.InitContainers[0]

	if cpu := p.Resources.Requests.Cpu(); cpu.MilliValue() != 25 {
		t.Errorf("proxy cpu request: want 25m, got %s", cpu)
	}
	if mem := p.Resources.Requests.Memory(); mem.Value() != 32*1024*1024 {
		t.Errorf("proxy memory request: want 32Mi, got %s", mem)
	}
	if mem := p.Resources.Limits.Memory(); mem.Value() != 64*1024*1024 {
		t.Errorf("proxy memory limit: want 64Mi, got %s", mem)
	}
	if _, ok := p.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("proxy cpu limit: want none, got %s", p.Resources.Limits.Cpu())
	}
}

func TestBuildRevision_TopologySpread(t *testing.T) {
	t.Parallel()
	spec := buildRevision(testRequest(), Config{}, "web-00001").Spec.Template.Spec

	if len(spec.TopologySpreadConstraints) != 1 {
		t.Fatalf("TopologySpreadConstraints: want 1, got %+v", spec.TopologySpreadConstraints)
	}
	c := spec.TopologySpreadConstraints[0]
	if c.MaxSkew != 1 || c.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("spread: want maxSkew 1 over hostname, got skew=%d key=%s", c.MaxSkew, c.TopologyKey)
	}
	// Soft: spreading a revision's replicas must never block scheduling.
	if c.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("whenUnsatisfiable: want ScheduleAnyway, got %s", c.WhenUnsatisfiable)
	}
	// Scoped to the revision's OWN pods: pack across deployments, spread within one.
	if c.LabelSelector == nil || !reflect.DeepEqual(c.LabelSelector.MatchLabels, map[string]string{LabelRevision: "web-00001"}) {
		t.Errorf("labelSelector: want revision label alone, got %+v", c.LabelSelector)
	}
}

func TestBuildPDB(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		replicas    int
		autoscaling *deployment.Autoscaling
		want        bool
	}{
		"fixed 1":           {replicas: 1, want: false},
		"fixed 3":           {replicas: 3, want: true},
		"autoscaling min 0": {replicas: 3, autoscaling: &deployment.Autoscaling{MinReplicas: 0}, want: false},
		"autoscaling min 1": {replicas: 1, autoscaling: &deployment.Autoscaling{MinReplicas: 1}, want: false},
		"autoscaling min 2": {replicas: 1, autoscaling: &deployment.Autoscaling{MinReplicas: 2}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := testRequest()
			req.Replicas = tc.replicas
			req.Autoscaling = tc.autoscaling
			pdb := buildPDB(req, "web-00001")
			if (pdb != nil) != tc.want {
				t.Fatalf("buildPDB: want pdb=%v, got %+v", tc.want, pdb)
			}
		})
	}
}

func TestBuildPDB_Shape(t *testing.T) {
	t.Parallel()
	req := testRequest() // Replicas: 2, no autoscaling
	pdb := buildPDB(req, "web-00001")
	if pdb == nil {
		t.Fatal("want a PDB for a fixed 2-replica deployment")
	}
	if pdb.Name != "dep-web-00001" {
		t.Errorf("Name: got %s", pdb.Name)
	}
	if pdb.Labels[LabelManagedBy] != ManagedByValue || pdb.Labels[LabelDeploymentID] != "web" || pdb.Labels[LabelRevision] != "web-00001" {
		t.Errorf("labels: got %v", pdb.Labels)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable: want 1, got %v", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.Selector == nil || !reflect.DeepEqual(pdb.Spec.Selector.MatchLabels, map[string]string{LabelRevision: "web-00001"}) {
		t.Errorf("selector: want revision label alone, got %+v", pdb.Spec.Selector)
	}
	if pdb.Spec.UnhealthyPodEvictionPolicy == nil || *pdb.Spec.UnhealthyPodEvictionPolicy != policyv1.AlwaysAllow {
		t.Errorf("unhealthyPodEvictionPolicy: want AlwaysAllow, got %v", pdb.Spec.UnhealthyPodEvictionPolicy)
	}
}

func TestBuildRevision_WorkerNoCommandNoResources(t *testing.T) {
	t.Parallel()
	req := &deployment.Request{ID: "bare", Image: "nginx", Port: 80}

	d := buildRevision(req, Config{}, "bare-00001")

	w := d.Spec.Template.Spec.Containers[0]
	if w.Command != nil {
		t.Errorf("Command: want nil (image entrypoint), got %v", w.Command)
	}
	if len(w.Resources.Limits) != 0 || len(w.Resources.Requests) != 0 {
		t.Errorf("Resources: want empty, got %+v", w.Resources)
	}
	if d.Spec.Replicas != 0 {
		t.Errorf("Replicas: want 0 for an unnormalised request, got %v", d.Spec.Replicas)
	}
	if d.Spec.ReadyTimeoutSeconds != 0 {
		t.Errorf("ReadyTimeoutSeconds: want 0, got %v", d.Spec.ReadyTimeoutSeconds)
	}
}

func TestBuildRevision_KubeletProbes(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Probes = &deployment.Probes{
		Liveness: &deployment.Probe{Path: "/live", PeriodMillis: 1500, TimeoutMillis: 200, FailureThreshold: 5},
		Startup:  &deployment.Probe{PeriodMillis: 1000},
	}

	w := buildRevision(req, Config{}, "web-00001").Spec.Template.Spec.Containers[0]

	live := w.LivenessProbe
	if live == nil || live.HTTPGet == nil || live.HTTPGet.Path != "/live" || live.HTTPGet.Port.IntValue() != 8080 {
		t.Fatalf("LivenessProbe: want GET /live:8080, got %+v", live)
	}
	// Millisecond fields round UP to whole seconds, min 1s.
	if live.PeriodSeconds != 2 || live.TimeoutSeconds != 1 || live.FailureThreshold != 5 {
		t.Errorf("liveness timing: got period=%d timeout=%d threshold=%d", live.PeriodSeconds, live.TimeoutSeconds, live.FailureThreshold)
	}

	start := w.StartupProbe
	if start == nil || start.TCPSocket == nil || start.TCPSocket.Port.IntValue() != 8080 {
		t.Fatalf("StartupProbe: want TCP :8080 (no path), got %+v", start)
	}
	if start.PeriodSeconds != 1 {
		t.Errorf("startup period: want 1, got %d", start.PeriodSeconds)
	}
}

func TestBuildRevision_NoProbes(t *testing.T) {
	t.Parallel()
	w := buildRevision(testRequest(), Config{}, "web-00001").Spec.Template.Spec.Containers[0]
	if w.LivenessProbe != nil || w.StartupProbe != nil {
		t.Errorf("want no kubelet probes, got liveness=%+v startup=%+v", w.LivenessProbe, w.StartupProbe)
	}
}

func TestBuildRevision_PersistentVolume(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Volumes = []volume.Volume{{Source: "state-pvc", Path: "/state"}}

	spec := buildRevision(req, Config{}, "web-00001").Spec.Template.Spec

	var claim string
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim != "state-pvc" {
		t.Errorf("pod should reference PVC state-pvc, got %q", claim)
	}
	w := spec.Containers[0]
	if !slices.ContainsFunc(w.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/state" }) {
		t.Errorf("worker should mount /state, got %+v", w.VolumeMounts)
	}
}

func TestBuildRevision_Artifacts(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Artifacts = []artifact.Artifact{
		&artifact.Write{ID: "w", In: "hello", Out: "index.html"},
	}

	spec := buildRevision(req, Config{SidecarImage: "sidecar:latest", JobSidecarImage: "artifact:latest"}, "web-00001").Spec.Template.Spec

	if len(spec.InitContainers) != 2 {
		t.Fatalf("InitContainers: want [artifact-pre proxy], got %d", len(spec.InitContainers))
	}
	pre := spec.InitContainers[0]
	if pre.Name != ContainerArtifactPre || spec.InitContainers[1].Name != ContainerProxy {
		t.Fatalf("init order: got %s, %s", pre.Name, spec.InitContainers[1].Name)
	}
	if pre.Image != "artifact:latest" {
		t.Errorf("artifact-pre image: want the job-sidecar artifact image, got %s", pre.Image)
	}
	if pre.RestartPolicy != nil {
		t.Errorf("artifact-pre RestartPolicy: want nil (plain init), got %v", *pre.RestartPolicy)
	}
	if !reflect.DeepEqual(pre.Args, []string{"-mode=pre"}) {
		t.Errorf("Args: got %v", pre.Args)
	}
	if !envHas(pre.Env, "JOB_ID", "dep-web") || !envHas(pre.Env, "SHARED_VOLUME_PATH", "/workspace") {
		t.Errorf("env missing JOB_ID/SHARED_VOLUME_PATH: %v", pre.Env)
	}
	artifactsJSON := envValue(pre.Env, "ARTIFACTS_JSON")
	if !strings.Contains(artifactsJSON, `"type"`) || !strings.Contains(artifactsJSON, "index.html") {
		t.Errorf("ARTIFACTS_JSON: got %s", artifactsJSON)
	}
}

func TestBuildRevision_SecurityFloor(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Artifacts = []artifact.Artifact{&artifact.Write{ID: "w", In: "x", Out: "y"}}
	cfg := Config{SidecarImage: "sidecar:latest", RunAsUser: 65532}

	spec := buildRevision(req, cfg, "web-00001").Spec.Template.Spec
	all := append(spec.InitContainers, spec.Containers...)
	if len(all) != 3 {
		t.Fatalf("want 3 containers, got %d", len(all))
	}

	for _, c := range all {
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("%s: missing SecurityContext", c.Name)
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s: runAsNonRoot not enforced", c.Name)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser != 65532 || sc.RunAsGroup == nil || *sc.RunAsGroup != 65532 {
			t.Errorf("%s: runAsUser/Group: got %v/%v", c.Name, sc.RunAsUser, sc.RunAsGroup)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: allowPrivilegeEscalation not disabled", c.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: capabilities not dropped: %+v", c.Name, sc.Capabilities)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: seccompProfile: got %+v", c.Name, sc.SeccompProfile)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: readOnlyRootFilesystem not enforced", c.Name)
		}
	}
}

func TestBuildService(t *testing.T) {
	t.Parallel()
	svc := buildService("web", "web-00001")

	if svc.Name != "dep-web-00001" {
		t.Errorf("Name: got %s", svc.Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Type: got %s", svc.Spec.Type)
	}
	// Selectorless: the endpointflip reconciler owns the EndpointSlice (ready
	// pods when warm, activator pods when cold).
	if svc.Spec.Selector != nil {
		t.Errorf("Selector: want none (endpoint-managed), got %v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != "http" || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 8000 {
		t.Errorf("Ports: want http 80→8000, got %+v", svc.Spec.Ports)
	}
	if svc.Labels[LabelManagedBy] != ManagedByValue || svc.Labels[LabelDeploymentID] != "web" || svc.Labels[LabelRevision] != "web-00001" {
		t.Errorf("labels: got %v", svc.Labels)
	}
}

func TestRevisionNaming(t *testing.T) {
	t.Parallel()
	if got := revisionName("web", 1); got != "web-00001" {
		t.Errorf("revisionName: got %s", got)
	}
	if got := revisionName("web", 123); got != "web-00123" {
		t.Errorf("revisionName: got %s", got)
	}
	for rev, want := range map[string]int{"web-00001": 1, "web-00123": 123, "my-app-00007": 7, "weird": 0, "web-x": 0} {
		if got := revisionNumber(rev); got != want {
			t.Errorf("revisionNumber(%s): want %d, got %d", rev, want, got)
		}
	}
	if next := revisionName("web", revisionNumber("web-00009")+1); next != "web-00010" {
		t.Errorf("mint next: got %s", next)
	}
}

func TestCeilSeconds(t *testing.T) {
	t.Parallel()
	for millis, want := range map[int]int32{1: 1, 999: 1, 1000: 1, 1001: 2, 1500: 2, 30000: 30} {
		if got := ceilSeconds(millis); got != want {
			t.Errorf("ceilSeconds(%d): want %d, got %d", millis, want, got)
		}
	}
}

// --- helpers ---

func envHas(env []corev1.EnvVar, name, value string) bool {
	return envValue(env, name) == value
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// A revision that asks to mount gets what a mount needs and nothing else does:
// the privilege is per-request, so an ordinary revision keeps its hardened
// sidecar. The workload is held by a startup probe until the mounts are in
// place, which is what makes the sidecar's mount visible to it in time.
func TestBuildRevision_MountCapabilityIsPerRequest(t *testing.T) {
	t.Parallel()
	base := func(arts ...artifact.Artifact) *deployment.Request {
		return &deployment.Request{ID: "web", Image: "app:v1", Port: 8080, Artifacts: arts}
	}

	plain := buildRevision(base(), Config{RunAsUser: 65532}, "web-00001").Spec.Template.Spec
	sidecar := containerNamed(t, plain, ContainerProxy)
	if sidecar.SecurityContext.Privileged != nil && *sidecar.SecurityContext.Privileged {
		t.Error("a revision that does not mount must not get a privileged sidecar")
	}
	if sidecar.StartupProbe != nil {
		t.Error("nothing to wait for: no startup probe without mounts")
	}
	if workspaceMountOf(t, sidecar).MountPropagation != nil {
		t.Error("no propagation without mounts")
	}
	if envOf(sidecar, workload.EnvArtifacts) != "" || envOf(sidecar, workload.EnvMounts) != "" {
		t.Error("the sidecar has no business knowing about artifacts it will not mount")
	}

	mounting := buildRevision(base(
		&artifact.Download{ID: "img", In: "https://acme.test/x.erofs", Out: "x.erofs"},
		&artifact.Mount{ID: "tree", In: "x.erofs", Out: "work", Depends: "img"},
	), Config{RunAsUser: 65532}, "web-00001").Spec.Template.Spec
	sidecar = containerNamed(t, mounting, ContainerProxy)
	worker := containerNamed(t, mounting, ContainerWorker)

	if sidecar.SecurityContext.Privileged == nil || !*sidecar.SecurityContext.Privileged {
		t.Error("mounting needs CAP_SYS_ADMIN: the sidecar must be privileged")
	}
	if p := workspaceMountOf(t, sidecar).MountPropagation; p == nil || *p != corev1.MountPropagationBidirectional {
		t.Errorf("sidecar propagation: got %v", p)
	}
	if p := workspaceMountOf(t, worker).MountPropagation; p == nil || *p != corev1.MountPropagationHostToContainer {
		t.Errorf("worker propagation: got %v", p)
	}
	// The kubelet holds the main containers until a native sidecar's startup
	// probe passes, which is the only barrier available here.
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.HTTPGet == nil ||
		sidecar.StartupProbe.HTTPGet.Path != workload.MountsReadyPath {
		t.Errorf("the workload must be gated on mounts being ready, got %+v", sidecar.StartupProbe)
	}
	if envOf(sidecar, workload.EnvMounts) != "true" {
		t.Error("the sidecar must be told it may mount")
	}
	if !strings.Contains(envOf(sidecar, workload.EnvArtifacts), `"mount"`) {
		t.Error("the sidecar must be told what to mount")
	}
}

func containerNamed(t *testing.T, spec corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %q", name)
	return corev1.Container{}
}

func workspaceMountOf(t *testing.T, c corev1.Container) corev1.VolumeMount {
	t.Helper()
	for _, m := range c.VolumeMounts {
		if m.Name == VolumeWorkspace {
			return m
		}
	}
	t.Fatalf("container %q does not mount the workspace", c.Name)
	return corev1.VolumeMount{}
}

func envOf(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
