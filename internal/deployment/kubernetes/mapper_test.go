package kubernetes

import (
	"encoding/json"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/deployment"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func testRequest() *deployment.Request {
	return &deployment.Request{
		ID:                      "web",
		Image:                   "nginx:1.27",
		Command:                 "nginx -g 'daemon off;'",
		CPU:                     0.5,
		Memory:                  128,
		Environment:             map[string]string{"FOO": "bar", "BAZ": "qux"},
		Host:                    "web.example.com",
		Port:                    8080,
		Replicas:                2,
		Concurrency:             4,
		TimeoutSeconds:          300,
		ProgressDeadlineSeconds: 600,
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

func TestBuildDeployment_BasicStructure(t *testing.T) {
	t.Parallel()
	req := testRequest()
	cfg := Config{SidecarImage: "sidecar:latest", ServiceAccount: "deployments", RunAsUser: 65532}

	d := buildDeployment(req, cfg, "web-00001")

	if d.Name != "dep-web-00001" {
		t.Errorf("Name: want dep-web-00001, got %s", d.Name)
	}
	for _, labels := range []map[string]string{d.Labels, d.Spec.Template.Labels} {
		if labels[LabelManagedBy] != ManagedByValue || labels[LabelDeploymentID] != "web" || labels[LabelRevision] != "web-00001" {
			t.Errorf("labels: got %v", labels)
		}
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 2 {
		t.Errorf("Replicas: want 2, got %v", d.Spec.Replicas)
	}
	if d.Spec.ProgressDeadlineSeconds == nil || *d.Spec.ProgressDeadlineSeconds != 600 {
		t.Errorf("ProgressDeadlineSeconds: want 600, got %v", d.Spec.ProgressDeadlineSeconds)
	}
	// Pod selector is the revision label ALONE — each immutable revision owns
	// exactly its own pods.
	if !reflect.DeepEqual(d.Spec.Selector.MatchLabels, map[string]string{LabelRevision: "web-00001"}) {
		t.Errorf("Selector: got %v", d.Spec.Selector.MatchLabels)
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

func TestBuildDeployment_ProxySidecar(t *testing.T) {
	t.Parallel()
	req := testRequest()
	cfg := Config{SidecarImage: "sidecar:latest", SidecarImagePullPolicy: "Never", RunAsUser: 65532}

	p := buildDeployment(req, cfg, "web-00001").Spec.Template.Spec.InitContainers[0]

	if p.Image != "sidecar:latest" || p.ImagePullPolicy != corev1.PullNever {
		t.Errorf("image/pull: got %s/%s", p.Image, p.ImagePullPolicy)
	}
	if p.RestartPolicy == nil || *p.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("RestartPolicy: want Always (native sidecar), got %v", p.RestartPolicy)
	}
	if len(p.Args) != 0 {
		t.Errorf("Args: want none (dedicated deployments-sidecar binary), got %v", p.Args)
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

func TestBuildDeployment_ProxyReadinessEnv(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Probes = &deployment.Probes{
		Readiness: &deployment.Probe{Path: "/healthz", PeriodMillis: 250, TimeoutMillis: 100, FailureThreshold: 5},
	}

	p := buildDeployment(req, Config{}, "web-00001").Spec.Template.Spec.InitContainers[0]

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

func TestBuildDeployment_Worker(t *testing.T) {
	t.Parallel()
	req := testRequest()
	cfg := Config{WorkerImagePullPolicy: "IfNotPresent"}

	w := buildDeployment(req, cfg, "web-00001").Spec.Template.Spec.Containers[0]

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

	if cpu := w.Resources.Limits.Cpu(); cpu.MilliValue() != 500 {
		t.Errorf("cpu limit: want 500m, got %s", cpu)
	}
	if mem := w.Resources.Limits.Memory(); mem.Value() != 128*1024*1024 {
		t.Errorf("memory limit: want 128Mi, got %s", mem)
	}
	if !reflect.DeepEqual(w.Resources.Requests, w.Resources.Limits) {
		t.Errorf("requests != limits: %+v", w.Resources)
	}
}

func TestBuildDeployment_WorkerNoCommandNoResources(t *testing.T) {
	t.Parallel()
	req := &deployment.Request{ID: "bare", Image: "nginx", Port: 80}

	d := buildDeployment(req, Config{}, "bare-00001")

	w := d.Spec.Template.Spec.Containers[0]
	if w.Command != nil {
		t.Errorf("Command: want nil (image entrypoint), got %v", w.Command)
	}
	if len(w.Resources.Limits) != 0 || len(w.Resources.Requests) != 0 {
		t.Errorf("Resources: want empty, got %+v", w.Resources)
	}
	if d.Spec.Replicas != nil {
		t.Errorf("Replicas: want nil (K8s default 1), got %v", *d.Spec.Replicas)
	}
	if d.Spec.ProgressDeadlineSeconds != nil {
		t.Errorf("ProgressDeadlineSeconds: want nil (K8s default), got %v", *d.Spec.ProgressDeadlineSeconds)
	}
}

func TestBuildDeployment_KubeletProbes(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Probes = &deployment.Probes{
		Liveness: &deployment.Probe{Path: "/live", PeriodMillis: 1500, TimeoutMillis: 200, FailureThreshold: 5},
		Startup:  &deployment.Probe{PeriodMillis: 1000},
	}

	w := buildDeployment(req, Config{}, "web-00001").Spec.Template.Spec.Containers[0]

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

func TestBuildDeployment_NoProbes(t *testing.T) {
	t.Parallel()
	w := buildDeployment(testRequest(), Config{}, "web-00001").Spec.Template.Spec.Containers[0]
	if w.LivenessProbe != nil || w.StartupProbe != nil {
		t.Errorf("want no kubelet probes, got liveness=%+v startup=%+v", w.LivenessProbe, w.StartupProbe)
	}
}

func TestBuildDeployment_Artifacts(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Artifacts = []artifact.Artifact{
		&artifact.Write{ID: "w", In: "hello", Out: "index.html"},
	}

	spec := buildDeployment(req, Config{SidecarImage: "sidecar:latest", JobSidecarImage: "artifact:latest"}, "web-00001").Spec.Template.Spec

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

func TestBuildDeployment_SecurityFloor(t *testing.T) {
	t.Parallel()
	req := testRequest()
	req.Artifacts = []artifact.Artifact{&artifact.Write{ID: "w", In: "x", Out: "y"}}
	cfg := Config{SidecarImage: "sidecar:latest", RunAsUser: 65532}

	spec := buildDeployment(req, cfg, "web-00001").Spec.Template.Spec
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
