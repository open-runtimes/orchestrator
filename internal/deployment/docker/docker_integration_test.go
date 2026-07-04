//go:build integration

package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/deployment"
	"os"
	"testing"
	"time"
)

func sidecarTestImage() string {
	if img := os.Getenv("SIDECAR_IMAGE"); img != "" {
		return img
	}
	return "ko.local/deployments-sidecar:latest"
}

func artifactTestImage() string {
	if img := os.Getenv("ARTIFACT_IMAGE"); img != "" {
		return img
	}
	return "ko.local/job-sidecar:latest"
}

func newTestOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()

	o, err := NewOrchestrator(t.Context(), Config{SidecarImage: sidecarTestImage(), ArtifactImage: artifactTestImage()})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	t.Cleanup(func() { o.Close() })

	if err := o.Start(t.Context()); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}
	return o
}

// containerIDsByType maps deployment.type label -> container ID for a deployment.
func containerIDsByType(t *testing.T, o *Orchestrator, id string) map[string]string {
	t.Helper()

	summaries, err := o.containersFor(t.Context(), id)
	if err != nil {
		t.Fatalf("Failed to list containers: %v", err)
	}
	ids := make(map[string]string, len(summaries))
	for _, c := range summaries {
		ids[c.Labels[labelType]] = c.ID
	}
	return ids
}

func TestDeployment_ApplyServeUpdateDelete(t *testing.T) {
	ctx := t.Context()
	o := newTestOrchestrator(t)

	id := fmt.Sprintf("it-serve-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = o.Delete(context.WithoutCancel(ctx), id) })

	req := &deployment.Request{
		ID:                      id,
		Image:                   "traefik/whoami:latest",
		CPU:                     1,
		Memory:                  128,
		Port:                    80,
		ProgressDeadlineSeconds: 60,
	}
	if err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to apply deployment: %v", err)
	}

	// Poll until the proxy is healthy and routable.
	var endpoints []*url.URL
	testutil.MustWaitFor(t, func() bool {
		endpoints, _ = o.Endpoints(ctx, id)
		return len(endpoints) > 0
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(500*time.Millisecond))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoints[0].String(), nil)
	if err != nil {
		t.Fatalf("Failed to build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Failed to GET endpoint %s: %v", endpoints[0], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", endpoints[0], resp.StatusCode)
	}

	status, err := o.Status(ctx, id)
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if status.State != deployment.StateReady || status.AvailableReplicas != 1 {
		t.Errorf("Status = %s (available %d), want ready/1", status.State, status.AvailableReplicas)
	}

	// Applying the identical spec must be a no-op: same containers.
	before := containerIDsByType(t, o, id)
	if err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to re-apply identical spec: %v", err)
	}
	if after := containerIDsByType(t, o, id); !maps.Equal(before, after) {
		t.Errorf("Identical apply replaced containers: before %v, after %v", before, after)
	}

	// A changed spec must replace the containers.
	req.Environment = map[string]string{"CHANGED": "1"}
	if err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to apply changed spec: %v", err)
	}
	after := containerIDsByType(t, o, id)
	if after[typeWorker] == before[typeWorker] || after[typeProxy] == before[typeProxy] {
		t.Errorf("Changed apply kept containers: before %v, after %v", before, after)
	}

	// Spec reads back the last-applied request.
	spec, err := o.Spec(ctx, id)
	if err != nil {
		t.Fatalf("Failed to get spec: %v", err)
	}
	if spec.Environment["CHANGED"] != "1" {
		t.Errorf("Spec environment = %v, want CHANGED=1", spec.Environment)
	}

	if err := o.Delete(ctx, id); err != nil {
		t.Fatalf("Failed to delete deployment: %v", err)
	}
	if _, err := o.Status(ctx, id); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("Status after delete = %v, want not found", err)
	}
}

func TestDeployment_NeverReadyFails(t *testing.T) {
	ctx := t.Context()
	o := newTestOrchestrator(t)

	id := fmt.Sprintf("it-neverready-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = o.Delete(context.WithoutCancel(ctx), id) })

	req := &deployment.Request{
		ID:                      id,
		Image:                   "alpine:latest",
		Command:                 "sleep 300",
		CPU:                     1,
		Memory:                  64,
		Port:                    80,
		ProgressDeadlineSeconds: 5,
	}
	if err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to apply deployment: %v", err)
	}

	var status *deployment.StatusResponse
	testutil.MustWaitFor(t, func() bool {
		var err error
		status, err = o.Status(ctx, id)
		if err != nil {
			return false
		}
		return status.State == deployment.StateFailed
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(time.Second))

	if status.AvailableReplicas != 0 {
		t.Errorf("AvailableReplicas = %d, want 0", status.AvailableReplicas)
	}
	if status.Error == "" {
		t.Error("Expected a failure reason on a never-ready deployment")
	}
}
