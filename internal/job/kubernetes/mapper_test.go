package kubernetes

import (
	"orchestrator/internal/job"
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
