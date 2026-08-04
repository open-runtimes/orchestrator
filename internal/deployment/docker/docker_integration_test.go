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
	if img := os.Getenv("DEPLOYMENT_SIDECAR_IMAGE"); img != "" {
		return img
	}
	return "ko.local/deployments-sidecar:latest"
}

func jobSidecarTestImage() string {
	if img := os.Getenv("JOB_SIDECAR_IMAGE"); img != "" {
		return img
	}
	return "ko.local/job-sidecar:latest"
}

func newTestOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()

	o, err := NewOrchestrator(t.Context(), Config{SidecarImage: sidecarTestImage(), JobSidecarImage: jobSidecarTestImage()})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	t.Cleanup(func() { o.Close() })

	if err := o.Start(t.Context()); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}
	return o
}

// waitForState polls Status until it reports the wanted state.
func waitForState(t *testing.T, o *Orchestrator, id, want string) {
	t.Helper()

	testutil.MustWaitFor(t, func() bool {
		status, err := o.Status(t.Context(), id)
		return err == nil && status.State == want
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(500*time.Millisecond))
}

// mustGetOK asserts a GET to the endpoint returns 200.
func mustGetOK(t *testing.T, endpoint *url.URL) {
	t.Helper()

	httpReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint.String(), nil)
	if err != nil {
		t.Fatalf("Failed to build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Failed to GET endpoint %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", endpoint, resp.StatusCode)
	}
}

// requireIdle asserts a scaled-to-zero deployment: no containers (checked via
// the Docker API by label), idle status, no endpoints.
func requireIdle(t *testing.T, o *Orchestrator, id string) {
	t.Helper()
	ctx := t.Context()

	summaries, err := o.containersFor(ctx, id)
	if err != nil {
		t.Fatalf("Failed to list containers: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("Expected no containers, found %d", len(summaries))
	}

	status, err := o.Status(ctx, id)
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if status.State != deployment.StateIdle || status.DesiredReplicas != 0 || status.AvailableReplicas != 0 {
		t.Errorf("Status = %s (desired %d, available %d), want idle/0/0",
			status.State, status.DesiredReplicas, status.AvailableReplicas)
	}

	endpoints, err := o.Endpoints(ctx, id)
	if err != nil {
		t.Fatalf("Failed to get endpoints: %v", err)
	}
	if len(endpoints) != 0 {
		t.Errorf("Endpoints = %v, want empty", endpoints)
	}
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
		ID:                  id,
		Image:               "traefik/whoami:latest",
		CPU:                 1,
		Memory:              128,
		Port:                80,
		ReadyTimeoutSeconds: 60,
	}
	if _, err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to apply deployment: %v", err)
	}

	// Poll until the proxy is healthy and routable.
	var endpoints []*url.URL
	testutil.MustWaitFor(t, func() bool {
		endpoints, _ = o.Endpoints(ctx, id)
		return len(endpoints) > 0
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(500*time.Millisecond))

	mustGetOK(t, endpoints[0])

	status, err := o.Status(ctx, id)
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if status.State != deployment.StateReady || status.AvailableReplicas != 1 {
		t.Errorf("Status = %s (available %d), want ready/1", status.State, status.AvailableReplicas)
	}

	// Applying the identical spec must be a no-op: same containers.
	before := containerIDsByType(t, o, id)
	if _, err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to re-apply identical spec: %v", err)
	}
	if after := containerIDsByType(t, o, id); !maps.Equal(before, after) {
		t.Errorf("Identical apply replaced containers: before %v, after %v", before, after)
	}

	// A changed spec must replace the containers.
	req.Environment = map[string]string{"CHANGED": "1"}
	if _, err := o.Apply(ctx, req); err != nil {
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

func TestDeployment_ScaleToZeroAndBack(t *testing.T) {
	ctx := t.Context()
	o := newTestOrchestrator(t)

	id := fmt.Sprintf("it-scale-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = o.Delete(context.WithoutCancel(ctx), id) })

	req := &deployment.Request{
		ID:                  id,
		Image:               "traefik/whoami:latest",
		CPU:                 1,
		Memory:              128,
		Port:                80,
		ReadyTimeoutSeconds: 60,
		Autoscaling:         &deployment.Autoscaling{MinReplicas: 0},
	}
	if _, err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to apply deployment: %v", err)
	}
	waitForState(t, o, id, deployment.StateReady)

	if err := o.Scale(ctx, id, 0); err != nil {
		t.Fatalf("Failed to scale to zero: %v", err)
	}
	requireIdle(t, o, id)

	// Scaling to zero again is idempotent.
	if err := o.Scale(ctx, id, 0); err != nil {
		t.Fatalf("Repeated scale to zero: %v", err)
	}

	// Applying the identical spec must not wake an idle deployment.
	if _, err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Failed to re-apply identical spec: %v", err)
	}
	requireIdle(t, o, id)

	if err := o.Scale(ctx, id, 1); err != nil {
		t.Fatalf("Failed to scale back up: %v", err)
	}
	waitForState(t, o, id, deployment.StateReady)

	endpoints, err := o.Endpoints(ctx, id)
	if err != nil {
		t.Fatalf("Failed to get endpoints: %v", err)
	}
	if len(endpoints) == 0 {
		t.Fatal("Expected an endpoint after scaling back up")
	}
	mustGetOK(t, endpoints[0])

	if err := o.Delete(ctx, id); err != nil {
		t.Fatalf("Failed to delete deployment: %v", err)
	}
	if _, err := o.Status(ctx, id); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("Status after delete = %v, want not found", err)
	}
}

func TestDeployment_ScaleUnknownIsNotFound(t *testing.T) {
	o := newTestOrchestrator(t)

	if err := o.Scale(t.Context(), "it-scale-missing", 1); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("Scale(unknown) = %v, want not found", err)
	}
}

func TestDeployment_NeverReadyFails(t *testing.T) {
	ctx := t.Context()
	o := newTestOrchestrator(t)

	id := fmt.Sprintf("it-neverready-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = o.Delete(context.WithoutCancel(ctx), id) })

	req := &deployment.Request{
		ID:                  id,
		Image:               "alpine:latest",
		Command:             "sleep 300",
		CPU:                 1,
		Memory:              64,
		Port:                80,
		ReadyTimeoutSeconds: 5,
	}
	if _, err := o.Apply(ctx, req); err != nil {
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
