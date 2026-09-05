//go:build k8s_integration

// Package kubernetes — real-cluster integration tests.
//
// These tests expect a running kind cluster named "orchestrator-dev" with the
// ko.local/workload-sidecar image loaded (same setup as the jobs backend's
// k8s_integration tests, so they share a CI job). Run via:
//
//	go test -race -tags=k8s_integration -timeout=10m ./internal/deployment/kubernetes/...
//
// The build tag keeps them out of `task test` (no cluster available in unit
// test runs).
//
// HTTPRoute assertions are guarded by a Gateway API CRD-existence check: on a
// cluster without the CRDs the orchestrator runs with GatewayEnabled=false
// and the route-specific tests skip, so the rest of the suite still passes.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/deployment"
	"orchestrator/internal/testutil"
	"orchestrator/internal/workload"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	testNamespace   = "orchestrator-integration"
	sidecarImage    = "ko.local/workload-sidecar:latest"
	jobSidecarImage = "ko.local/job-sidecar:latest"

	// agnhost is a static binary that runs fine as uid 65532 with a read-only
	// rootfs — unlike most web-server images, which want root or a writable /.
	workerImage = "registry.k8s.io/e2e-test-images/agnhost:2.47"
)

// setup brings up an Orchestrator wired to the host's default kubeconfig
// (pinned to kind-orchestrator-dev), creates the test namespace if needed,
// disables the gateway when the cluster lacks the Gateway API CRDs, and
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
		GatewayEnabled:         true,
	}
	o, err := NewOrchestrator(ctx, cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	if !gatewayCRDsPresent(o) {
		t.Log("Gateway API CRDs absent — running with GatewayEnabled=false")
		o.cfg.GatewayEnabled = false
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

	// teardown stops the reconcilers and deletes all managed objects
	// (Revisions with their pods, Services, markers, HTTPRoutes),
	// so successive runs don't pollute each other.
	teardown := func() {
		_ = o.Close()
		ctx := context.Background()
		managed := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue}
		if revisions, err := o.revisions.List(ctx, testNamespace, managed); err == nil {
			for i := range revisions.Items {
				_ = o.revisions.Delete(ctx, testNamespace, revisions.Items[i].Name, metav1.DeleteOptions{})
			}
		}
		_ = o.client.CoreV1().Pods(testNamespace).DeleteCollection(ctx, metav1.DeleteOptions{}, managed)
		_ = o.client.CoreV1().ConfigMaps(testNamespace).DeleteCollection(ctx, metav1.DeleteOptions{}, managed)
		_ = o.client.CoreV1().Secrets(testNamespace).DeleteCollection(ctx, metav1.DeleteOptions{}, managed)
		// Services and HTTPRoutes don't support DeleteCollection; one by one.
		if svcs, err := o.client.CoreV1().Services(testNamespace).List(ctx, managed); err == nil {
			for i := range svcs.Items {
				_ = o.client.CoreV1().Services(testNamespace).Delete(ctx, svcs.Items[i].Name, metav1.DeleteOptions{})
			}
		}
		if !o.cfg.GatewayEnabled {
			return
		}
		if routes, err := o.gateway.GatewayV1().HTTPRoutes(testNamespace).List(ctx, managed); err == nil {
			for i := range routes.Items {
				_ = o.gateway.GatewayV1().HTTPRoutes(testNamespace).Delete(ctx, routes.Items[i].Name, metav1.DeleteOptions{})
			}
		}
	}
	return o, teardown
}

// gatewayCRDsPresent reports whether the cluster serves the Gateway API v1
// group (HTTPRoute CRD installed).
func gatewayCRDsPresent(o *Orchestrator) bool {
	resources, err := o.client.Discovery().ServerResourcesForGroupVersion(gatewayv1.GroupVersion.String())
	if err != nil {
		return false
	}
	for _, r := range resources.APIResources {
		if r.Kind == "HTTPRoute" {
			return true
		}
	}
	return false
}

// requireGateway skips the test when the cluster has no Gateway API CRDs.
func requireGateway(t *testing.T, o *Orchestrator) {
	t.Helper()
	if !o.cfg.GatewayEnabled {
		t.Skip("Gateway API CRDs not installed; skipping HTTPRoute assertions")
	}
}

func serverRequest(id string) *deployment.Request {
	return &deployment.Request{
		ID:                  id,
		Image:               workerImage,
		Command:             "/agnhost netexec --http-port=8080",
		CPU:                 0.1,
		Memory:              64,
		Hosts:               []string{id + ".example.com"},
		Port:                8080,
		Replicas:            1,
		TimeoutSeconds:      60,
		ReadyTimeoutSeconds: 120,
	}
}

// waitForState polls Status until the deployment reaches the wanted state.
func waitForState(t *testing.T, o *Orchestrator, id, state string, timeout time.Duration) {
	t.Helper()
	testutil.MustWaitFor(t, func() bool {
		s, err := o.Status(t.Context(), id)
		return err == nil && s.State == state
	}, testutil.WithTimeout(timeout), testutil.WithInterval(time.Second))
}

// waitForCut polls the marker until the rollout reconciler records rev as
// last-ready.
func waitForCut(t *testing.T, o *Orchestrator, id, rev string, timeout time.Duration) {
	t.Helper()
	testutil.MustWaitFor(t, func() bool {
		m, err := o.getMarker(t.Context(), id)
		return err == nil && m.LastReady == rev
	}, testutil.WithTimeout(timeout), testutil.WithInterval(time.Second))
}

// --- happy path: apply, ready, revisions, rollout, delete ---

// TestIntegration_RevisionLifecycle walks the full revision lifecycle:
// Apply → 00001 ready + auto-cut, no-op re-Apply (no new revision), changed
// re-Apply (mints 00002, traffic cuts once ready), Delete → NotFound.
func TestIntegration_RevisionLifecycle(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()

	id := fmt.Sprintf("web-%d", time.Now().UnixNano()%1_000_000_000)
	rev1, rev2 := revisionName(id, 1), revisionName(id, 2)

	if _, err := o.Apply(t.Context(), serverRequest(id)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Ready within 120s (image pull included), then the reconciler cuts.
	waitForState(t, o, id, deployment.StateReady, 120*time.Second)
	waitForCut(t, o, id, rev1, 30*time.Second)

	// One ready proxy endpoint on the routed revision. (Pod IPs aren't
	// reachable from the host on kind, so we assert presence, not HTTP.)
	endpoints, err := o.Endpoints(t.Context(), id)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("endpoints: want 1, got %d (%v)", len(endpoints), endpoints)
	}

	// The revision's Service exists.
	if _, err := o.client.CoreV1().Services(testNamespace).Get(t.Context(), objectNameFor(rev1), metav1.GetOptions{}); err != nil {
		t.Fatalf("expected revision Service: %v", err)
	}

	// Applying the identical spec mints nothing and touches no workload.
	before, err := o.revisions.Get(t.Context(), testNamespace, objectNameFor(rev1))
	if err != nil {
		t.Fatalf("get Revision: %v", err)
	}
	if _, err := o.Apply(t.Context(), serverRequest(id)); err != nil {
		t.Fatalf("no-op Apply: %v", err)
	}
	after, err := o.revisions.Get(t.Context(), testNamespace, objectNameFor(rev1))
	if err != nil {
		t.Fatalf("get Revision: %v", err)
	}
	if after.Generation != before.Generation {
		t.Errorf("no-op Apply bumped Generation %d → %d", before.Generation, after.Generation)
	}
	if m, _ := o.getMarker(t.Context(), id); m.LatestRevision != rev1 {
		t.Errorf("no-op Apply minted %s", m.LatestRevision)
	}

	// A changed spec mints an immutable NEW revision; the old one keeps
	// serving until the new one is ready, then the auto-cut shifts traffic.
	changed := serverRequest(id)
	changed.Environment = map[string]string{"FOO": "bar"}
	if _, err := o.Apply(t.Context(), changed); err != nil {
		t.Fatalf("changed Apply: %v", err)
	}
	waitForCut(t, o, id, rev2, 120*time.Second)

	s, err := o.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(s.Revisions) < 2 || s.Revisions[0] != rev2 {
		t.Errorf("revisions newest-first: got %v", s.Revisions)
	}
	if len(s.Traffic) != 1 || s.Traffic[0].RevisionName != rev2 || s.Traffic[0].Percent != 100 {
		t.Errorf("traffic after cut: got %+v", s.Traffic)
	}

	// Spec round-trips through the marker.
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

// TestIntegration_HTTPRouteShape asserts the reconciled HTTPRoute: hostname,
// parentRef, and the two rules with per-backendRef X-Revision set filters.
func TestIntegration_HTTPRouteShape(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()
	requireGateway(t, o)

	id := fmt.Sprintf("route-%d", time.Now().UnixNano()%1_000_000_000)
	rev1 := revisionName(id, 1)
	if _, err := o.Apply(t.Context(), serverRequest(id)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	route, err := o.gateway.GatewayV1().HTTPRoutes(testNamespace).Get(t.Context(), objectNameFor(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get HTTPRoute: %v", err)
	}
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != id+".example.com" {
		t.Errorf("hostnames: got %v", route.Spec.Hostnames)
	}
	if len(route.Spec.ParentRefs) != 1 || string(route.Spec.ParentRefs[0].Name) != o.cfg.GatewayName {
		t.Errorf("parentRefs: got %+v", route.Spec.ParentRefs)
	}
	if len(route.Spec.Rules) != 2 {
		t.Fatalf("rules: want 2 (async + default), got %d", len(route.Spec.Rules))
	}
	async, dflt := route.Spec.Rules[0], route.Spec.Rules[1]
	if len(async.Matches) != 1 || len(async.Matches[0].Headers) != 1 ||
		string(async.Matches[0].Headers[0].Name) != "Prefer" || async.Matches[0].Headers[0].Value != workload.PreferAsyncPattern {
		t.Errorf("async match: got %+v", async.Matches)
	}
	if len(async.BackendRefs) != 1 || string(async.BackendRefs[0].Name) != o.cfg.ActivatorService {
		t.Errorf("async backendRefs must target the activator: %+v", async.BackendRefs)
	}
	if len(dflt.BackendRefs) != 1 || string(dflt.BackendRefs[0].Name) != objectNameFor(rev1) {
		t.Errorf("default backendRefs: got %+v", dflt.BackendRefs)
	}
	for _, refs := range [][]gatewayv1.HTTPBackendRef{async.BackendRefs, dflt.BackendRefs} {
		filters := refs[0].Filters
		if len(filters) != 1 || filters[0].RequestHeaderModifier == nil ||
			len(filters[0].RequestHeaderModifier.Set) != 1 ||
			string(filters[0].RequestHeaderModifier.Set[0].Name) != "X-Revision" ||
			filters[0].RequestHeaderModifier.Set[0].Value != rev1 {
			t.Errorf("X-Revision set filter: got %+v", filters)
		}
	}
}

// TestIntegration_TrafficSplitAndRollback pins a canary split across two
// revisions and rolls back, asserting the weights land on the route.
func TestIntegration_TrafficSplitAndRollback(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()
	requireGateway(t, o)

	id := fmt.Sprintf("canary-%d", time.Now().UnixNano()%1_000_000_000)
	rev1, rev2 := revisionName(id, 1), revisionName(id, 2)

	if _, err := o.Apply(t.Context(), serverRequest(id)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	waitForCut(t, o, id, rev1, 120*time.Second)
	changed := serverRequest(id)
	changed.Environment = map[string]string{"V": "2"}
	if _, err := o.Apply(t.Context(), changed); err != nil {
		t.Fatalf("changed Apply: %v", err)
	}
	waitForCut(t, o, id, rev2, 120*time.Second)

	split := []deployment.Target{
		{RevisionName: rev2, Percent: 90},
		{RevisionName: rev1, Percent: 10},
	}
	if err := o.SetTraffic(t.Context(), id, split); err != nil {
		t.Fatalf("SetTraffic split: %v", err)
	}
	s, err := o.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(s.Traffic) != 2 || s.Traffic[0].Percent != 90 || s.Traffic[1].Percent != 10 {
		t.Errorf("split traffic: got %+v", s.Traffic)
	}
	if m, _ := o.getMarker(t.Context(), id); m.TrafficMode != trafficModeManual {
		t.Errorf("split must pin manual mode, got %s", m.TrafficMode)
	}

	// Rollback: 100% to the old revision, still manual.
	if err := o.SetTraffic(t.Context(), id, []deployment.Target{{RevisionName: rev1, Percent: 100}}); err != nil {
		t.Fatalf("SetTraffic rollback: %v", err)
	}
	s, err = o.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(s.Traffic) != 1 || s.Traffic[0].RevisionName != rev1 {
		t.Errorf("rollback traffic: got %+v", s.Traffic)
	}
}

// --- failure path: never ready ---

// TestIntegration_NeverReadyFails deploys a worker that exits immediately and
// asserts the deployment reaches failed once spec.readyTimeoutSeconds
// elapses — and that the failed revision is never cut to.
func TestIntegration_NeverReadyFails(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()

	id := fmt.Sprintf("crash-%d", time.Now().UnixNano()%1_000_000_000)
	req := serverRequest(id)
	req.Command = "exit 1"
	req.ReadyTimeoutSeconds = 15

	if _, err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	waitForState(t, o, id, deployment.StateFailed, 90*time.Second)

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
	// Rollout protection: the failed revision never became last-ready.
	if m, _ := o.getMarker(t.Context(), id); m.LastReady != "" {
		t.Errorf("failed revision must not be cut to, lastReady=%s", m.LastReady)
	}
}

// TestIntegration_ScaleToZeroAndBack exercises the 0↔1 cycle the activator
// and idle-to-zero loop drive in production: ready → Scale(0) → idle with no
// endpoints → Scale(1) → ready again.
func TestIntegration_ScaleToZeroAndBack(t *testing.T) {
	o, teardown := setup(t)
	defer teardown()

	id := fmt.Sprintf("zero-%d", time.Now().UnixNano()%1_000_000_000)
	req := serverRequest(id)
	req.Autoscaling = &deployment.Autoscaling{MinReplicas: 0}
	if _, err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	waitForState(t, o, id, deployment.StateReady, 120*time.Second)

	if err := o.Scale(t.Context(), id, 0); err != nil {
		t.Fatalf("Scale(0): %v", err)
	}
	waitForState(t, o, id, deployment.StateIdle, 60*time.Second)
	testutil.MustWaitFor(t, func() bool {
		endpoints, err := o.Endpoints(t.Context(), id)
		return err == nil && len(endpoints) == 0
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	if err := o.Scale(t.Context(), id, 1); err != nil {
		t.Fatalf("Scale(1): %v", err)
	}
	waitForState(t, o, id, deployment.StateReady, 120*time.Second)

	endpoints, err := o.Endpoints(t.Context(), id)
	if err != nil || len(endpoints) != 1 {
		t.Fatalf("endpoints after wake: want 1, got %d (err=%v)", len(endpoints), err)
	}
}

func TestIntegration_RevisionPreparesBeforeCommand(t *testing.T) {
	for _, mount := range []bool{false, true} {
		t.Run(fmt.Sprintf("mount=%v", mount), func(t *testing.T) {
			o, teardown := setup(t)
			defer teardown()
			id := fmt.Sprintf("gate-%d", time.Now().UnixNano()%1_000_000_000)
			req := serverRequest(id)
			req.Artifacts = []artifact.Artifact{&artifact.Write{ID: "write", In: "prepared", Out: "index.html"}}
			dir := "/workspace"
			if mount {
				req.Artifacts = append(req.Artifacts,
					&artifact.Archive{ID: "archive", In: "index.html", Out: "site.tar.gz", Format: "tar", Compression: "gzip", Depends: "write"},
					&artifact.Mount{ID: "mount", In: "site.tar.gz", Out: "site", Depends: "archive"})
				dir += "/site"
			}
			req.Command = "test $(cat " + dir + "/index.html) = prepared && exec /agnhost netexec --http-port=8080"
			if _, err := o.Apply(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			waitForState(t, o, id, deployment.StateReady, 90*time.Second)
			pods, err := o.client.CoreV1().Pods(testNamespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=" + revisionName(id, 1)})
			if err != nil || len(pods.Items) != 1 {
				t.Fatalf("expected one revision pod: %v", err)
			}
			spec := pods.Items[0].Spec
			if len(spec.InitContainers) != 1 || spec.InitContainers[0].StartupProbe != nil {
				t.Fatal("revision must have one non-blocking resident sidecar")
			}
		})
	}
}
