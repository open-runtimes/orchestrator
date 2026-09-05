package kubernetes

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"orchestrator/internal/volume"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildJob_MountArtifact(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID: "job-mnt", Image: "alpine:3.20", TimeoutSeconds: 60, Workspace: "/workspace",
		Artifacts: []artifact.Artifact{
			&artifact.Mount{ID: "m", In: "data.sqfs", Out: "mnt/data"},
		},
	}

	// A mount artifact → privileged sidecar, propagation on sidecar + worker, startup probe.
	j := buildJob(req, OrchestratorConfig{Namespace: "orchestrator"}, "sidecar:latest")
	spec := j.Spec.Template.Spec
	sidecar := spec.InitContainers[0]
	if sidecar.SecurityContext == nil || sidecar.SecurityContext.Privileged == nil || !*sidecar.SecurityContext.Privileged {
		t.Error("sidecar should be privileged when mounting")
	}
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.Exec == nil {
		t.Error("sidecar should have a -check-ready startup probe")
	}
	if got := sidecar.VolumeMounts[0].MountPropagation; got == nil || *got != corev1.MountPropagationBidirectional {
		t.Errorf("sidecar mount propagation: want Bidirectional, got %v", got)
	}
	if got := spec.Containers[0].VolumeMounts[0].MountPropagation; got == nil || *got != corev1.MountPropagationHostToContainer {
		t.Errorf("worker mount propagation: want HostToContainer, got %v", got)
	}
}

func TestBuildJob_NoMount_Unprivileged(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-1", Image: "alpine:3.20", TimeoutSeconds: 60, Workspace: "/workspace"}

	j := buildJob(req, OrchestratorConfig{Namespace: "orchestrator"}, "sidecar:latest")
	spec := j.Spec.Template.Spec
	sidecar := spec.InitContainers[0]
	if sidecar.SecurityContext != nil {
		t.Error("non-mount job should not get privilege")
	}
	if sidecar.VolumeMounts[0].MountPropagation != nil || spec.Containers[0].VolumeMounts[0].MountPropagation != nil {
		t.Error("non-mount job should not set mount propagation")
	}
}

func TestBuildJob_PersistentVolume(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID: "job-vol", Image: "alpine:3.20", TimeoutSeconds: 60, Workspace: "/workspace",
		Volumes: []volume.Volume{{Source: "data-pvc", Path: "/data", ReadOnly: true}},
	}

	j := buildJob(req, OrchestratorConfig{Namespace: "orchestrator"}, "sidecar:latest")
	spec := j.Spec.Template.Spec

	// Pod carries the PVC volume alongside the workspace emptyDir.
	var claim string
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim != "data-pvc" {
		t.Errorf("pod should reference PVC data-pvc, got %q", claim)
	}

	// Worker gets the mount; the two sidecars do not (they operate on the workspace).
	worker := spec.Containers[0]
	if !slices.ContainsFunc(worker.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/data" && m.ReadOnly }) {
		t.Errorf("worker should mount /data read-only, got %+v", worker.VolumeMounts)
	}
	for _, c := range spec.InitContainers {
		if slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/data" }) {
			t.Errorf("sidecar %q should not mount the persistent volume", c.Name)
		}
	}

	// A persistent volume must NOT trigger the privileged squashfs-mount path.
	if sidecar := spec.InitContainers[0]; sidecar.SecurityContext != nil {
		t.Error("persistent volume should not make the sidecar privileged")
	}
}

// --- watchConfigFromRequest ---

func TestWatchConfigFromRequest_NoCallback(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-1", Image: "alpine:latest"}

	cfg := watchConfigFromRequest(req)

	if cfg.jobID != "job-1" {
		t.Errorf("jobID: want job-1, got %s", cfg.jobID)
	}
	if cfg.image != "alpine:latest" {
		t.Errorf("image: want alpine:latest, got %s", cfg.image)
	}
	if cfg.dest != nil {
		t.Error("dest: want nil when no callback configured")
	}
}

func TestWatchConfigFromRequest_WithCallback(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID:    "job-1",
		Image: "alpine:latest",
		Meta:  map[string]string{"tenant": "acme"},
		Callback: &job.Callback{
			URL:    "https://hooks.example.com/cb",
			Key:    "secret",
			Events: []string{"job.start", "job.exit"},
		},
	}
	cfg := watchConfigFromRequest(req)
	if cfg.dest == nil {
		t.Fatal("dest: want non-nil")
	}
	if cfg.dest.URL != "https://hooks.example.com/cb" {
		t.Errorf("dest.URL: got %s", cfg.dest.URL)
	}
	if cfg.dest.Key != "secret" {
		t.Errorf("dest.Key: got %s", cfg.dest.Key)
	}
	if !reflect.DeepEqual(cfg.dest.Events, []string{"job.start", "job.exit"}) {
		t.Errorf("dest.Events: got %v", cfg.dest.Events)
	}
	if cfg.dest.Meta["tenant"] != "acme" {
		t.Errorf("dest.Meta[tenant]: got %s", cfg.dest.Meta["tenant"])
	}
}

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

func TestBuildJob_BasicStructure(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID:             "job-1",
		Image:          "alpine:3.20",
		Command:        "echo hello",
		CPU:            0.5,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Environment:    map[string]string{"FOO": "bar"},
		ArtifactToken:  "tok-1",
	}
	cfg := OrchestratorConfig{
		Namespace:                     "orchestrator",
		ServiceAccount:                "job-sidecar",
		JobRetention:                  15 * time.Minute,
		ArtifactEndpoint:              "http://jobs-service.orchestrator.svc:8080",
		TerminationGracePeriodSeconds: 600,
	}

	j := buildJob(req, cfg, "ko.local/job-sidecar:latest")

	if j.Name != "job-job-1" {
		t.Errorf("Name: want job-job-1, got %s", j.Name)
	}
	if j.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("managed-by label: got %s", j.Labels[LabelManagedBy])
	}
	if j.Labels[LabelJobID] != "job-1" {
		t.Errorf("job.id label: got %s", j.Labels[LabelJobID])
	}
	if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit: want 0, got %v", j.Spec.BackoffLimit)
	}
	if j.Spec.TTLSecondsAfterFinished == nil || *j.Spec.TTLSecondsAfterFinished != int32((15*time.Minute).Seconds()) {
		t.Errorf("TTLSecondsAfterFinished: got %v", j.Spec.TTLSecondsAfterFinished)
	}

	spec := j.Spec.Template.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy: want Never, got %s", spec.RestartPolicy)
	}
	if spec.ServiceAccountName != "job-sidecar" {
		t.Errorf("ServiceAccountName: got %s", spec.ServiceAccountName)
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 600 {
		t.Errorf("TerminationGracePeriodSeconds: want 600, got %v", spec.TerminationGracePeriodSeconds)
	}

	if len(spec.InitContainers) != 1 {
		t.Fatalf("InitContainers: want 1, got %d", len(spec.InitContainers))
	}
	sidecar := spec.InitContainers[0]
	if sidecar.Name != ContainerSidecar || !slices.Contains(sidecar.Args, "-mode=combined") {
		t.Fatalf("expected combined sidecar, got %+v", sidecar)
	}
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("combined sidecar must use restartPolicy Always")
	}
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.Exec == nil ||
		!reflect.DeepEqual(sidecar.StartupProbe.Exec.Command, []string{"/ko-app/job-sidecar", "-check-ready"}) {
		t.Fatal("worker must wait for artifacts and mounts, including jobs without mounts")
	}
	if j.Spec.ActiveDeadlineSeconds == nil || *j.Spec.ActiveDeadlineSeconds != 60 {
		t.Fatal("job deadline must bound native sidecar setup retries")
	}

	if len(spec.Containers) != 1 {
		t.Fatalf("Containers: want 1, got %d", len(spec.Containers))
	}
	worker := spec.Containers[0]
	if worker.Name != ContainerWorker {
		t.Errorf("worker.Name: want %s, got %s", ContainerWorker, worker.Name)
	}
	if worker.Image != "alpine:3.20" {
		t.Errorf("worker.Image: got %s", worker.Image)
	}
	if !reflect.DeepEqual(worker.Command, []string{"/bin/sh", "-c", "echo hello"}) {
		t.Errorf("worker.Command: got %v", worker.Command)
	}
	if worker.WorkingDir != "/workspace" {
		t.Errorf("worker.WorkingDir: got %s", worker.WorkingDir)
	}

	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != VolumeWorkspace || spec.Volumes[0].EmptyDir == nil {
		t.Errorf("Volumes: want one emptyDir %q, got %+v", VolumeWorkspace, spec.Volumes)
	}

	// Both containers must mount the workspace at req.Workspace.
	for _, c := range []corev1.Container{sidecar, worker} {
		found := false
		for _, m := range c.VolumeMounts {
			if m.Name == VolumeWorkspace && m.MountPath == "/workspace" {
				found = true
			}
		}
		if !found {
			t.Errorf("container %s missing workspace mount", c.Name)
		}
	}

	// Sidecar env carries job metadata for HTTP reporting.
	if !envHas(sidecar.Env, "JOB_ID", "job-1") {
		t.Errorf("sidecar.Env missing JOB_ID=job-1: %v", sidecar.Env)
	}
	if !envHas(sidecar.Env, "TIMEOUT_SECONDS", strconv.Itoa(60)) {
		t.Errorf("sidecar.Env missing TIMEOUT_SECONDS=60: %v", sidecar.Env)
	}
	if !envHas(sidecar.Env, "ARTIFACT_ENDPOINT", cfg.ArtifactEndpoint) {
		t.Errorf("sidecar.Env missing ARTIFACT_ENDPOINT: %v", sidecar.Env)
	}

	// The artifact token authenticates the sidecar containers only — it must
	// never reach the worker, and no SA token may be mounted for the worker
	// to read pod annotations with.
	if !envHas(sidecar.Env, "ARTIFACT_TOKEN", "tok-1") {
		t.Errorf("sidecar.Env missing ARTIFACT_TOKEN: %v", sidecar.Env)
	}
	for _, e := range worker.Env {
		if e.Name == "ARTIFACT_TOKEN" {
			t.Errorf("worker.Env must not contain ARTIFACT_TOKEN: %v", worker.Env)
		}
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken: want false, got %v", spec.AutomountServiceAccountToken)
	}

	// Worker env carries the user-supplied environment.
	if !envHas(worker.Env, "FOO", "bar") {
		t.Errorf("worker.Env missing FOO=bar: %v", worker.Env)
	}
}

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
	j := buildJob(req, OrchestratorConfig{Namespace: "orchestrator"}, "sidecar:latest")

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

func TestBuildJob_WatchConfigRoundTrip(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID:    "job-3",
		Image: "alpine:3.20",
		Meta:  map[string]string{"x": "y"},
		Callback: &job.Callback{
			URL:    "https://cb",
			Key:    "k",
			Events: []string{"job.exit"},
		},
	}
	j := buildJob(req, OrchestratorConfig{Namespace: "orchestrator"}, "sidecar:latest")

	cfg := watchConfigFromJob(j)
	if cfg.jobID != "job-3" {
		t.Errorf("jobID: got %s", cfg.jobID)
	}
	if cfg.image != "alpine:3.20" {
		t.Errorf("image: got %s", cfg.image)
	}
	if cfg.dest == nil || cfg.dest.URL != "https://cb" || cfg.dest.Key != "k" {
		t.Errorf("dest round-trip failed: %+v", cfg.dest)
	}
	if cfg.dest.Meta["x"] != "y" {
		t.Errorf("meta round-trip: got %v", cfg.dest.Meta)
	}
}

func TestBuildJob_EmptyWorkspaceDefaults(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-4", Image: "alpine:latest"}
	j := buildJob(req, OrchestratorConfig{}, "sidecar:latest")

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
	cfg := OrchestratorConfig{Tolerations: []corev1.Toleration{{Key: "workload", Value: "edge-builds", Effect: corev1.TaintEffectNoSchedule}}}
	got := buildJob(req, cfg, "sidecar:latest").Spec.Template.Spec.Tolerations
	if len(got) != 1 || got[0].Key != "workload" {
		t.Errorf("tolerations: want workload=edge-builds:NoSchedule, got %+v", got)
	}
}

// Ensure buildJob does not panic on zero resources — Requests/Limits just stay empty.
func TestBuildJob_NoResources(t *testing.T) {
	t.Parallel()
	req := &job.Request{ID: "job-5", Image: "alpine:latest"}
	j := buildJob(req, OrchestratorConfig{}, "sidecar:latest")
	res := j.Spec.Template.Spec.Containers[0].Resources
	if len(res.Limits) != 0 || len(res.Requests) != 0 {
		t.Errorf("expected empty resources, got %+v", res)
	}
}

// --- helpers ---

func envHas(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}

func TestBuildJob_DefaultDeadlineAndEntrypoint(t *testing.T) {
	req := &job.Request{ID: "default", Image: "custom:latest"}
	j := buildJob(req, OrchestratorConfig{}, "sidecar:latest")
	if j.Spec.ActiveDeadlineSeconds == nil || *j.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatal("missing default deadline for sidecar retries")
	}
	if len(j.Spec.Template.Spec.Containers[0].Command) != 0 {
		t.Fatal("must preserve the image entrypoint when command is omitted")
	}
}
