//go:build k8s_integration

// Package kubernetes — real-cluster integration tests for the pools backend.
//
// These tests expect a running kind cluster named "orchestrator-dev" with the
// ko.local/workload-sidecar and ko.local/pool-shim images loaded (same
// setup as the deployments backend's k8s_integration tests, so they share a
// CI job). Run via:
//
//	go test -race -tags=k8s_integration -timeout=10m ./internal/pool/kubernetes/...
//
// The build tag keeps them out of `task test` (no cluster available in unit
// test runs).
//
// The claim protocol is an HTTP POST from this process to the warm pod's IP,
// so the suite additionally needs the kind pod network to be routable from
// the host (true for Linux CI; not for Docker Desktop). A reachability guard
// skips the suite where it isn't — mirroring the deployments package's
// Gateway CRD guard.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/testutil"
	"orchestrator/internal/warm"
	"orchestrator/pkg/pool"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	itNamespace    = "orchestrator-pools-integration"
	itSidecarImage = "ko.local/workload-sidecar:latest"
	itShimImage    = "ko.local/pool-shim:latest"

	// agnhost is a static binary that runs fine as uid 65532 with a read-only
	// rootfs — and ships a shell for the shim's `sh -c` exec.
	itPoolImage = "registry.k8s.io/e2e-test-images/agnhost:2.47"
)

// itSetup brings up an Orchestrator for the given pools, wired to the host's
// default kubeconfig (pinned to kind-orchestrator-dev), creates the test
// namespace if needed, and registers teardown.
func itSetup(t *testing.T, pools ...pool.Pool) *Orchestrator {
	t.Helper()
	ctx := t.Context()

	cfg := Config{
		SidecarImage: itSidecarImage,
		ShimImage:    itShimImage,
		Pools:        pools,
		// Pin the kubeconfig context explicitly: this test must only ever run
		// against the project's kind cluster, never the user's current-context.
		Context:                "kind-orchestrator-dev",
		Namespace:              itNamespace,
		SidecarImagePullPolicy: "Never", // ko-loaded images live locally in kind
		GatewayEnabled:         true,
	}
	o, err := NewOrchestrator(ctx, cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	if !itGatewayCRDsPresent(o) {
		t.Log("Gateway API CRDs absent — running with GatewayEnabled=false")
		o.cfg.GatewayEnabled = false
	}

	_, err = o.client.CoreV1().Namespaces().Get(ctx, itNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: itNamespace}}
		if _, err := o.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace: %v", err)
		}
	} else if err != nil {
		t.Fatalf("get namespace: %v", err)
	}

	if err := o.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Teardown stops the control loop first (so replenishment cannot race the
	// deletes), then removes all managed objects so successive runs don't
	// pollute each other.
	t.Cleanup(func() {
		_ = o.Close()
		ctx := context.Background()
		managed := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue}
		_ = o.client.CoreV1().Pods(itNamespace).DeleteCollection(ctx, metav1.DeleteOptions{}, managed)
		if svcs, err := o.client.CoreV1().Services(itNamespace).List(ctx, managed); err == nil {
			for i := range svcs.Items {
				_ = o.client.CoreV1().Services(itNamespace).Delete(ctx, svcs.Items[i].Name, metav1.DeleteOptions{})
			}
		}
		if !o.cfg.GatewayEnabled {
			return
		}
		if routes, err := o.gateway.GatewayV1().HTTPRoutes(itNamespace).List(ctx, managed); err == nil {
			for i := range routes.Items {
				_ = o.gateway.GatewayV1().HTTPRoutes(itNamespace).Delete(ctx, routes.Items[i].Name, metav1.DeleteOptions{})
			}
		}
	})
	return o
}

// itGatewayCRDsPresent reports whether the cluster serves the Gateway API v1
// group (HTTPRoute CRD installed).
func itGatewayCRDsPresent(o *Orchestrator) bool {
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

// waitWarm polls Pools until the pool reports at least `want` warm-ready
// pods (image pull included on the first run).
func waitWarm(t *testing.T, o *Orchestrator, poolID string, want int, timeout time.Duration) {
	t.Helper()
	testutil.MustWaitFor(t, func() bool {
		statuses, err := o.Pools(t.Context())
		if err != nil {
			return false
		}
		for _, s := range statuses {
			if s.ID == poolID {
				return s.Warm >= want
			}
		}
		return false
	}, testutil.WithTimeout(timeout), testutil.WithInterval(time.Second))
}

// requirePodNetwork skips the test when warm pod IPs are not routable from
// the host (the claim POST would never arrive).
func requirePodNetwork(t *testing.T, o *Orchestrator, poolID string) {
	t.Helper()
	pods, err := o.warm.Pods(t.Context(), poolID)
	if err != nil {
		t.Fatalf("poolPods: %v", err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for i := range pods {
		if !o.warm.Claimable(&pods[i]) {
			continue
		}
		reachable := testutil.WaitFor(t, func() bool {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, warm.AdminURL(pods[i].Status.PodIP, "/ready"), nil)
			if err != nil {
				return false
			}
			resp, err := client.Do(req)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return true
		}, testutil.WithTimeout(10*time.Second), testutil.WithInterval(time.Second))
		if !reachable {
			t.Skipf("pod IP %s not routable from the host; skipping (claims are direct pod HTTP)", pods[i].Status.PodIP)
		}
		return
	}
	t.Fatal("no claimable warm pod to probe")
}

func itPool(id string) pool.Pool {
	return pool.Pool{
		ID:     id,
		Image:  itPoolImage,
		Port:   8080,
		Size:   1,
		CPU:    0.1,
		Memory: 64,
		Burst:  pool.BurstReject,
	}
}

// itServeCommand starts agnhost's HTTP echo server on the pool port — the
// serving workload every activation late-binds onto a warm pod.
const itServeCommand = "/agnhost netexec --http-port=8080"

// TestIntegration_HTTPActivation is the pools happy path: a real warm pod
// (shim installed, sidecar armed) is claimed, the serving command execs, and
// the activation turns ready at its URL.
func TestIntegration_HTTPActivation(t *testing.T) {
	o := itSetup(t, itPool("it-http"))
	waitWarm(t, o, "it-http", 1, 120*time.Second)
	requirePodNetwork(t, o, "it-http")

	status, err := o.Activate(t.Context(), "it-http", &pool.Activation{
		ID:      "serve1",
		Command: itServeCommand,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if status.State != pool.StateReady || status.URL == "" {
		t.Fatalf("want ready with an URL, got %s %q (error %q)", status.State, status.URL, status.Error)
	}

	// The live activation stays queryable from the labeled pod.
	got, err := o.Status(t.Context(), "it-http", "serve1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.State != pool.StateReady || got.URL != status.URL {
		t.Errorf("Status after activation: got %s %q", got.State, got.URL)
	}
}

// TestIntegration_ClaimRace fires three concurrent activations at a 1-pod
// reject pool: the sidecar serializes, so exactly one wins and the racing
// losers get 409 → no other pod → 429 (Exhausted).
func TestIntegration_ClaimRace(t *testing.T) {
	o := itSetup(t, itPool("it-race"))
	waitWarm(t, o, "it-race", 1, 120*time.Second)
	requirePodNetwork(t, o, "it-race")

	var mu sync.Mutex
	var wins, rejects int
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			status, err := o.Activate(t.Context(), "it-race", &pool.Activation{
				ID:      fmt.Sprintf("race-%d", i),
				Command: itServeCommand,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && status.State == pool.StateReady:
				wins++
			case errors.Is(err, apperrors.ErrExhausted):
				rejects++
			default:
				t.Errorf("unexpected outcome: status %+v err %v", status, err)
			}
		})
	}
	wg.Wait()

	if wins != 1 || rejects != 2 {
		t.Errorf("want 1 winner and 2 rejects, got %d/%d", wins, rejects)
	}
}

// TestIntegration_ReplenishAfterClaim checks the slot comes back off the
// request path: after an activation consumes the only warm pod, the control
// loop mints a fresh one (a NEW pod — claimed pods are never resold).
func TestIntegration_ReplenishAfterClaim(t *testing.T) {
	o := itSetup(t, itPool("it-replenish"))
	waitWarm(t, o, "it-replenish", 1, 120*time.Second)
	requirePodNetwork(t, o, "it-replenish")

	status, err := o.Activate(t.Context(), "it-replenish", &pool.Activation{
		ID:      "consume",
		Command: itServeCommand,
	})
	if err != nil || status.State != pool.StateReady {
		t.Fatalf("Activate: status %+v err %v", status, err)
	}
	claimedPod := status.PodID

	testutil.MustWaitFor(t, func() bool {
		pods, err := o.warm.Pods(t.Context(), "it-replenish")
		if err != nil {
			return false
		}
		for i := range pods {
			if o.warm.Claimable(&pods[i]) && pods[i].Name != claimedPod {
				return true
			}
		}
		return false
	}, testutil.WithTimeout(120*time.Second), testutil.WithInterval(time.Second))
}
