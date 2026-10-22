package kubernetes

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"orchestrator/internal/mountagent"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// --- callbackDestFromAnnotations ---

func TestCallbackDestFromAnnotations_Empty(t *testing.T) {
	t.Parallel()
	if got := callbackDestFromAnnotations(nil); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
	if got := callbackDestFromAnnotations(map[string]string{}); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}

func TestCallbackDestFromAnnotations_RoundTrip(t *testing.T) {
	t.Parallel()
	ann := map[string]string{
		AnnotationCallbackURL:    "https://hooks.example.com/cb",
		AnnotationCallbackKey:    "k",
		AnnotationCallbackEvents: "job.start,job.exit",
		AnnotationMeta:           `{"tenant":"acme"}`,
	}
	dest := callbackDestFromAnnotations(ann)
	if dest == nil {
		t.Fatal("dest: want non-nil")
	}
	if dest.URL != "https://hooks.example.com/cb" {
		t.Errorf("URL: got %s", dest.URL)
	}
	if dest.Key != "k" {
		t.Errorf("Key: got %s", dest.Key)
	}
	if !reflect.DeepEqual(dest.Events, []string{"job.start", "job.exit"}) {
		t.Errorf("Events: got %v", dest.Events)
	}
	if dest.Meta["tenant"] != "acme" {
		t.Errorf("Meta[tenant]: got %s", dest.Meta["tenant"])
	}
}

// --- buildJob ---

func TestBuildJob_CallbackAnnotations(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID:    "job-2",
		Image: "alpine:latest",
		Meta:  map[string]string{"tenant": "acme"},
		Callback: &job.Callback{
			URL:    "https://hooks.example.com/cb",
			Key:    "secret",
			Events: []string{"job.start", "job.exit"},
		},
	}
	j := buildJob(req, Config{Namespace: "orchestrator"}, "sidecar:latest")

	if j.Annotations[AnnotationCallbackURL] != "https://hooks.example.com/cb" {
		t.Errorf("url annotation: got %s", j.Annotations[AnnotationCallbackURL])
	}
	if j.Annotations[AnnotationCallbackKey] != "secret" {
		t.Errorf("key annotation: got %s", j.Annotations[AnnotationCallbackKey])
	}
	if j.Annotations[AnnotationCallbackEvents] != "job.start,job.exit" {
		t.Errorf("events annotation: got %s", j.Annotations[AnnotationCallbackEvents])
	}
	if !strings.Contains(j.Annotations[AnnotationMeta], "acme") {
		t.Errorf("meta annotation missing tenant: %s", j.Annotations[AnnotationMeta])
	}
}

func TestBuildJob_EmptyWorkspaceDefaults(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-4", Image: "alpine:latest"}
	j := buildJob(req, Config{}, "sidecar:latest")

	worker := j.Spec.Template.Spec.Containers[0]
	if worker.WorkingDir != "/workspace" {
		t.Errorf("default workspace: got %s", worker.WorkingDir)
	}
	for _, m := range worker.VolumeMounts {
		if m.Name == VolumeWorkspace && m.MountPath != "/workspace" {
			t.Errorf("mount path default: got %s", m.MountPath)
		}
	}
}

func TestBuildJob_Tolerations(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-6", Image: "alpine:latest"}
	cfg := Config{Tolerations: []corev1.Toleration{{Key: "workload", Value: "edge-builds", Effect: corev1.TaintEffectNoSchedule}}}
	got := buildJob(req, cfg, "sidecar:latest").Spec.Template.Spec.Tolerations
	if len(got) != 1 || got[0].Key != "workload" {
		t.Errorf("tolerations: want workload=edge-builds:NoSchedule, got %+v", got)
	}
}

// The worker runs the user's command, so it keeps root and a writable image, but
// gets the runtime's seccomp profile, no new privileges and no raw sockets.
func TestBuildJob_WorkerSecurityContext(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-7", Image: "alpine:latest", Command: "npm install"}
	sc := buildJob(req, Config{}, "sidecar:latest").Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("worker: missing SecurityContext")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccomp: want RuntimeDefault, got %+v", sc.SeccompProfile)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("allowPrivilegeEscalation: want false, got %v", sc.AllowPrivilegeEscalation)
	}
	if sc.Capabilities == nil || !reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"NET_RAW"}) {
		t.Errorf("capabilities.drop: want [NET_RAW], got %+v", sc.Capabilities)
	}
	if sc.RunAsNonRoot != nil || sc.RunAsUser != nil || sc.ReadOnlyRootFilesystem != nil {
		t.Errorf("worker must keep the image's user and a writable root: got %+v", sc)
	}
}

// Ensure buildJob does not panic on zero resources — Requests/Limits just stay empty.
func TestBuildJob_NoResources(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-5", Image: "alpine:latest"}
	j := buildJob(req, Config{}, "sidecar:latest")
	res := j.Spec.Template.Spec.Containers[0].Resources
	if len(res.Limits) != 0 || len(res.Requests) != 0 {
		t.Errorf("expected empty resources, got %+v", res)
	}
}

// --- helpers ---

func TestBuildJob_DefaultDeadlineAndEntrypoint(t *testing.T) {
	req := &job.Request{ID: "default", Image: "custom:latest"}
	j := buildJob(req, Config{}, "sidecar:latest")
	if j.Spec.ActiveDeadlineSeconds == nil || *j.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatal("missing default deadline for sidecar retries")
	}
	if len(j.Spec.Template.Spec.Containers[0].Command) != 0 {
		t.Fatal("must preserve the image entrypoint when command is omitted")
	}
}

// --- mount modes ---

func mountRequest() *job.Request {
	return &job.Request{
		ID:    "job-mount",
		Image: "node:22",
		Artifacts: []artifact.Artifact{
			&artifact.Download{ID: "img", In: "https://example.com/code.erofs", Out: "code.erofs"},
			&artifact.Mount{ID: "code", In: "code.erofs", Out: "code", Depends: "img"},
		},
	}
}

func containerByName(spec corev1.PodSpec, name string) corev1.Container {
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		if c.Name == name {
			return c
		}
	}
	return corev1.Container{}
}

func envValue(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// In agent mode the pod that mounts carries no privileged container at all,
// and the token the agent will accept for its workspace reaches the sidecar
// and nobody else.
func TestBuildJob_AgentMountMode(t *testing.T) {
	t.Parallel()
	cfg := Config{MountAgentPort: 9999}
	spec := buildJob(mountRequest(), cfg, "sidecar:latest").Spec.Template.Spec

	for _, c := range append(spec.InitContainers, spec.Containers...) {
		if sc := c.SecurityContext; sc != nil && sc.Privileged != nil && *sc.Privileged {
			t.Errorf("container %s is privileged in agent mode", c.Name)
		}
	}

	ws := spec.Volumes[0]
	if ws.Name != VolumeWorkspace || ws.CSI == nil || ws.CSI.Driver != mountagent.Driver {
		t.Fatalf("workspace volume = %+v, want an inline volume of the agent's driver", ws.VolumeSource)
	}
	token := ws.CSI.VolumeAttributes[mountagent.AttributeToken]
	if len(token) < 26 {
		t.Fatalf("workspace token = %q, want at least 128 random bits", token)
	}

	sidecar := containerByName(spec, ContainerSidecar)
	if got, _ := envValue(sidecar, mountagent.EnvToken); got.Value != token {
		t.Errorf("sidecar token = %q, want the volume's %q", got.Value, token)
	}
	if got, _ := envValue(sidecar, mountagent.EnvEndpoint); got.Value != "http://$(HOST_IP):9999" {
		t.Errorf("sidecar endpoint = %q", got.Value)
	}
	if got, ok := envValue(sidecar, "HOST_IP"); !ok || got.ValueFrom == nil || got.ValueFrom.FieldRef.FieldPath != "status.hostIP" {
		t.Errorf("HOST_IP must come from the downward API, got %+v", got)
	}
	for _, name := range []string{ContainerWorker, ContainerArtifactPre} {
		c := containerByName(spec, name)
		if _, ok := envValue(c, mountagent.EnvToken); ok {
			t.Errorf("%s was given the workspace token", name)
		}
	}

	// Both receive the agent's mounts; neither propagates its own outward.
	for _, name := range []string{ContainerSidecar, ContainerWorker} {
		c := containerByName(spec, name)
		if p := c.VolumeMounts[0].MountPropagation; p == nil || *p != corev1.MountPropagationHostToContainer {
			t.Errorf("%s workspace propagation = %v, want HostToContainer", name, p)
		}
	}

	// Two pods never share a token.
	other := buildJob(mountRequest(), cfg, "sidecar:latest").Spec.Template.Spec.Volumes[0].CSI.VolumeAttributes[mountagent.AttributeToken]
	if other == token {
		t.Error("two jobs were minted the same workspace token")
	}
}

// The default stays exactly what it was: the sidecar mounts, privileged, and
// propagates outward to the worker.
func TestBuildJob_SidecarMountMode(t *testing.T) {
	t.Parallel()
	spec := buildJob(mountRequest(), Config{}, "sidecar:latest").Spec.Template.Spec
	if spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("workspace = %+v, want an emptyDir", spec.Volumes[0].VolumeSource)
	}
	sidecar := containerByName(spec, ContainerSidecar)
	if sc := sidecar.SecurityContext; sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Error("sidecar must be privileged to mount in sidecar mode")
	}
	if p := sidecar.VolumeMounts[0].MountPropagation; p == nil || *p != corev1.MountPropagationBidirectional {
		t.Errorf("sidecar propagation = %v, want Bidirectional", p)
	}
	if _, ok := envValue(sidecar, mountagent.EnvToken); ok {
		t.Error("sidecar mode handed out a workspace token")
	}
}

// Agent mode changes nothing for a job that mounts nothing.
func TestBuildJob_AgentModeWithoutMounts(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "plain", Image: "alpine"}
	spec := buildJob(req, Config{MountAgentPort: 1}, "sidecar:latest").Spec.Template.Spec
	if spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("workspace = %+v, want an emptyDir", spec.Volumes[0].VolumeSource)
	}
	if _, ok := envValue(containerByName(spec, ContainerSidecar), mountagent.EnvToken); ok {
		t.Error("a job without mounts was handed a workspace token")
	}
}
