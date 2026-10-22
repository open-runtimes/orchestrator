package kubernetes

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestCallback_ReadsTheJobsOwnDestination(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, noopWatcher{})
	defer o.Close()

	req := &job.Request{
		ID: "job-1", Image: "alpine:latest", TimeoutSeconds: 60, Workspace: "/workspace",
		Callback: &job.Callback{URL: "http://callbacks.test/hook", Key: "job-key", Events: []string{"orchestrator.job.artifact"}},
		Meta:     map[string]string{"projectId": "p1"},
	}
	if err := o.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	dest, err := o.Callback(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if dest == nil || dest.URL != "http://callbacks.test/hook" || dest.Key != "job-key" || dest.Meta["projectId"] != "p1" {
		t.Errorf("Callback = %+v, want the destination the job was created with", dest)
	}

	if _, err := o.Callback(context.Background(), "job-2"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("Callback(unknown) error = %v, want not found", err)
	}
}

// The sidecar reports results; it holds no callback destination, signing key
// or meta.
func TestSidecarEnv_CarriesNoCallbackConfig(t *testing.T) {
	t.Parallel()
	req := &job.Request{
		ID:       "job-1",
		Callback: &job.Callback{URL: "http://callbacks.test/hook", Key: "job-key", Events: []string{"orchestrator.job.artifact"}},
		Meta:     map[string]string{"projectId": "p1"},
	}
	for _, e := range sidecarEnv(req, "http://jobs:8080", "/workspace") {
		if strings.HasPrefix(e.Name, "CALLBACK_") || e.Name == "JOB_META" || strings.Contains(e.Value, "job-key") {
			t.Errorf("sidecar env carries %s=%q", e.Name, e.Value)
		}
	}
}

// A sidecar with no s3:// artifact has no reason to hold the platform's
// object-store credentials.
func TestSidecarEnv_S3OnlyWithS3Artifacts(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("S3_SECRET_ACCESS_KEY", "s3cr3t")

	withS3 := &job.Request{ID: "job-1", Artifacts: []artifact.Artifact{
		&artifact.Upload{ID: "out", In: "out", Out: "s3://bucket/key"},
	}}
	if !hasEnvPrefix(sidecarEnv(withS3, "http://jobs:8080", "/workspace"), "S3_") {
		t.Error("s3 artifact: sidecar env should carry S3 credentials")
	}

	httpOnly := &job.Request{ID: "job-1", Artifacts: []artifact.Artifact{
		&artifact.Download{ID: "src", In: "https://example.test/src.tgz", Out: "src.tgz"},
	}}
	if hasEnvPrefix(sidecarEnv(httpOnly, "http://jobs:8080", "/workspace"), "S3_") {
		t.Error("http-only artifacts: sidecar env must not carry S3 credentials")
	}
}

func hasEnvPrefix(env []corev1.EnvVar, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e.Name, prefix) {
			return true
		}
	}
	return false
}
