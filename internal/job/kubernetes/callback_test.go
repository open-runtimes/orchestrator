package kubernetes

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/job"
	"strings"
	"testing"
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
