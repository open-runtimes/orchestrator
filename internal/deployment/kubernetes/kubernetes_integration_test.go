//go:build k8s_integration

// Package kubernetes — real-cluster integration tests.
//
// These tests expect a running kind cluster named "orchestrator-dev" with the
// ko.local/job-sidecar image loaded (same setup as the jobs backend's
// k8s_integration tests, so they share a CI job). Run via:
//
//	go test -race -tags=k8s_integration -timeout=10m ./internal/deployment/kubernetes/...
//
// The build tag keeps them out of `task test` (no cluster available in unit
// test runs).
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/deployment"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testNamespace   = "orchestrator-integration"
	sidecarImage    = "ko.local/deployments-sidecar:latest"
	jobSidecarImage = "ko.local/job-sidecar:latest"

	// agnhost is a static binary that runs fine as uid 65532 with a read-only
	// rootfs — unlike most web-server images, which want root or a writable /.
	workerImage = "registry.k8s.io/e2e-test-images/agnhost:2.47"
)

// setup brings up an Orchestrator wired to the host's default kubeconfig
// (pinned to kind-orchestrator-dev), creates the test namespace if needed, and
// returns teardown.
func setup(t *testing.T) (*Orchestrator, func()) {
	t.Helper()
	ctx := t.Context()

	cfg := Config{
		SidecarImage:    sidecarImage,
		JobSidecarImage: jobSidecarImage,
		// Pin the kubeconfig context explicitly: this test must only ever run
		// against the project's kind cluster, never the user's current-context.
		Context:                "kind-orchestrator-dev",
		Namespace:              testNamespace,
		SidecarImagePullPolicy: "Never", // ko-loaded image lives locally in kind
	}
	o, err := NewOrchestrator(ctx, cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}

	// Create the namespace if not present. Cluster-scoped Create; idempotent-ish.
	_, err = o.client.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
		if _, err := o.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace: %v", err)
		}
	} else if err != nil {
		t.Fatalf("get namespace: %v", err)
	}

	if err := o.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// teardown deletes all managed Deployments (and their pods via background
	// propagation) plus their Services, so successive runs don't pollute each
	// other. Services don't support DeleteCollection, so delete one by one.
	teardown := func() {
		ctx := context.Background()
		prop := metav1.DeletePropagationBackground
		_ = o.client.AppsV1().Deployments(testNamespace).DeleteCollection(
			ctx,
			metav1.DeleteOptions{PropagationPolicy: &prop},
			metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue},
		)
		svcs, err := o.client.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelManagedBy + "=" + ManagedByValue,
		})
		if err != nil {
			return
		}
		for i := range svcs.Items {
			_ = o.client.CoreV1().Services(testNamespace).Delete(ctx, svcs.Items[i].Name, metav1.DeleteOptions{})
		}
	}
	return o, teardown
}

func serverRequest(id string) *deployment.Request {
	return &deployment.Request{
		ID:                      id,
		Image:                   workerImage,
		Command:                 "/agnhost netexec --http-port=8080",
		CPU:                     0.1,
		Memory:                  64,
		Port:                    8080,
		Replicas:                1,
		TimeoutSeconds:          60,
		ProgressDeadlineSeconds: 120,
	}
}

// currentPodNames returns the names of live (non-terminating) pods backing the
// deployment.
func currentPodNames(t *testing.T, o *Orchestrator, id string) []string {
	t.Helper()
	pods, err := o.client.CoreV1().Pods(testNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: LabelDeploymentID + "=" + id,
	})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	var names []string
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			names = append(names, pods.Items[i].Name)
		}
	}
	return names
}

// --- happy path: apply, ready, endpoints, in-place update, delete ---

// TestIntegration_ApplyReadyRolloutDelete walks the full single-revision
// lifecycle: Apply → ready → Endpoints, no-op re-Apply (no rollout), changed
// re-Apply (rollout to a new pod), Delete → NotFound.
func TestIntegration_ApplyReadyRolloutDelete(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()

	id := fmt.Sprintf("web-%d", time.Now().UnixNano())
	req := serverRequest(id)
	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Ready within 120s (image pull included).
	testutil.MustWaitFor(t, func() bool {
		s, err := o.Status(t.Context(), id)
		return err == nil && s.State == deployment.StateReady
	}, testutil.WithTimeout(120*time.Second), testutil.WithInterval(time.Second))

	// One ready proxy endpoint. (Pod IPs aren't reachable from the host on
	// kind, so we assert presence, not HTTP round-trips.)
	endpoints, err := o.Endpoints(t.Context(), id)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("endpoints: want 1, got %d (%v)", len(endpoints), endpoints)
	}

	// The fronting Service exists.
	if _, err := o.client.CoreV1().Services(testNamespace).Get(t.Context(), deploymentNameFor(id), metav1.GetOptions{}); err != nil {
		t.Fatalf("expected Service: %v", err)
	}

	// Applying the identical spec is a no-op: no generation bump, no rollout.
	before, err := o.client.AppsV1().Deployments(testNamespace).Get(t.Context(), deploymentNameFor(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	podsBefore := currentPodNames(t, o, id)
	if len(podsBefore) != 1 {
		t.Fatalf("pods before no-op apply: want 1, got %v", podsBefore)
	}
	if err := o.Apply(t.Context(), serverRequest(id)); err != nil {
		t.Fatalf("no-op Apply: %v", err)
	}
	after, err := o.client.AppsV1().Deployments(testNamespace).Get(t.Context(), deploymentNameFor(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if after.Generation != before.Generation {
		t.Errorf("no-op Apply bumped Generation %d → %d (rollout triggered)", before.Generation, after.Generation)
	}
	if pods := currentPodNames(t, o, id); len(pods) != 1 || pods[0] != podsBefore[0] {
		t.Errorf("no-op Apply changed pods: %v → %v", podsBefore, pods)
	}

	// A changed spec rolls out a new pod and converges back to ready.
	changed := serverRequest(id)
	changed.Environment = map[string]string{"FOO": "bar"}
	if err := o.Apply(t.Context(), changed); err != nil {
		t.Fatalf("changed Apply: %v", err)
	}
	testutil.MustWaitFor(t, func() bool {
		pods := currentPodNames(t, o, id)
		if len(pods) != 1 || pods[0] == podsBefore[0] {
			return false
		}
		s, err := o.Status(t.Context(), id)
		return err == nil && s.State == deployment.StateReady
	}, testutil.WithTimeout(120*time.Second), testutil.WithInterval(time.Second))

	// Spec round-trips through the backend.
	spec, err := o.Spec(t.Context(), id)
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	if spec.Environment["FOO"] != "bar" {
		t.Errorf("Spec env: want FOO=bar, got %v", spec.Environment)
	}

	// Delete tears everything down.
	if err := o.Delete(t.Context(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	testutil.MustWaitFor(t, func() bool {
		_, err := o.Status(t.Context(), id)
		return errors.Is(err, apperrors.ErrNotFound)
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))
}

// --- failure path: never ready ---

// TestIntegration_NeverReadyFails deploys a worker that exits immediately and
// asserts the deployment reaches failed once spec.progressDeadlineSeconds
// elapses without a ready replica.
func TestIntegration_NeverReadyFails(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()

	id := fmt.Sprintf("crash-%d", time.Now().UnixNano())
	req := serverRequest(id)
	req.Command = "exit 1"
	req.ProgressDeadlineSeconds = 15

	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		s, err := o.Status(t.Context(), id)
		return err == nil && s.State == deployment.StateFailed
	}, testutil.WithTimeout(90*time.Second), testutil.WithInterval(time.Second))

	s, err := o.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("final Status: %v", err)
	}
	if s.Error == "" {
		t.Error("failed status should carry the controller's condition message")
	}
	if s.AvailableReplicas != 0 {
		t.Errorf("available: want 0, got %d", s.AvailableReplicas)
	}
}
