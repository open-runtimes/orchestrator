//go:build k8s_integration

// Package kubernetes — real-cluster integration tests.
//
// These tests expect a running kind cluster named "orchestrator-dev" with the
// ko.local/jobs-service and ko.local/job-sidecar images loaded. Run via:
//
//	task test-k8s-integration
//
// The build tag keeps them out of `task test` (no cluster available in unit
// test runs).
package kubernetes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/job"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testNamespace = "orchestrator-integration"
	sidecarImage  = "ko.local/job-sidecar:latest"
)

// setup brings up an Orchestrator wired to the host's default kubeconfig
// (which points at kind-orchestrator-dev), creates a dedicated test namespace
// if needed, and returns teardown.
func setup(t *testing.T) (*Orchestrator, *job.CallbackEmitter, func()) {
	t.Helper()
	ctx := t.Context()

	emitter := job.NewCallbackEmitter()
	factory := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		// Pin the kubeconfig context explicitly: this test must only ever run
		// against the project's kind cluster, never the user's current-context.
		Context:                "kind-orchestrator-dev",
		Namespace:              testNamespace,
		SidecarImagePullPolicy: "Never", // ko-loaded image lives locally in kind
	})
	orch, err := factory(emitter)
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	o := orch.(*Orchestrator)

	// Create the namespace if not present. Cluster-scoped Create; idempotent-ish.
	_, err = o.client.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := o.client.CoreV1().Namespaces().Create(ctx, nsObject(testNamespace), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace: %v", err)
		}
	} else if err != nil {
		t.Fatalf("get namespace: %v", err)
	}

	// Create the job-sidecar ServiceAccount referenced by our Pod spec. In a
	// real install this comes from the Helm chart; the direct-wire tests need
	// it made by hand.
	_, err = o.client.CoreV1().ServiceAccounts(testNamespace).Get(ctx, "job-sidecar", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "job-sidecar", Namespace: testNamespace}}
		if _, err := o.client.CoreV1().ServiceAccounts(testNamespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create service account: %v", err)
		}
	} else if err != nil {
		t.Fatalf("get service account: %v", err)
	}

	if err := o.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	teardown := func() {
		// Drop all test Jobs AND their child Pods. Background propagation
		// (rather than Orphan, which is the Delete default) prevents stuck
		// Pods from lingering across test runs.
		prop := metav1.DeletePropagationBackground
		_ = o.client.BatchV1().Jobs(testNamespace).DeleteCollection(
			context.Background(),
			metav1.DeleteOptions{PropagationPolicy: &prop},
			metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue},
		)
		orch.Close()
	}
	return o, emitter, teardown
}

func nsObject(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// --- happy path ---

// TestIntegration_HappyPath runs a tiny alpine job end-to-end and verifies the
// full callback sequence (start, exit) lands on the user's webhook.
func TestIntegration_HappyPath(t *testing.T) {
	events, callbackURL, closeCallback := startCallbackServer(t)
	defer closeCallback()

	o, emitter, teardown := setup(t)
	defer teardown()

	d := wireDispatcher(t, emitter)
	defer d.Close(context.Background())

	jobID := fmt.Sprintf("happy-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:3.20",
		Command:        "echo 'hello from integration' && sleep 1",
		CPU:            0.1,
		Memory:         64,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Callback: &job.Callback{
			URL: callbackURL,
		},
	}
	if err := o.Run(t.Context(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait for job to reach terminal state via Status.
	testutil.MustWaitFor(t, func() bool {
		s, err := o.Status(t.Context(), jobID)
		if err != nil {
			return false
		}
		return s.State == job.StateCompleted || s.State == job.StateFailed
	}, testutil.WithTimeout(120*time.Second), testutil.WithInterval(time.Second))

	status, err := o.Status(t.Context(), jobID)
	if err != nil {
		t.Fatalf("final Status: %v", err)
	}
	if status.State != job.StateCompleted {
		t.Errorf("final state: want completed, got %s", status.State)
	}

	// Give the dispatcher a beat to deliver in-flight callbacks.
	testutil.MustWaitFor(t, func() bool {
		return events.has(job.CallbackTypeStart) && events.has(job.CallbackTypeExit)
	}, testutil.WithTimeout(10*time.Second))

	if !events.has(job.CallbackTypeStart) {
		t.Errorf("expected a %s callback", job.CallbackTypeStart)
	}
	if !events.has(job.CallbackTypeExit) {
		t.Errorf("expected a %s callback", job.CallbackTypeExit)
	}
}

// --- failure path ---

// TestIntegration_NonZeroExit runs a job whose worker exits with a non-zero
// code and verifies the Status maps to Failed and both start+exit callbacks
// fire. This exercises the most common failure shape (user's command bombed).
func TestIntegration_NonZeroExit(t *testing.T) {
	events, callbackURL, closeCallback := startCallbackServer(t)
	defer closeCallback()

	o, emitter, teardown := setup(t)
	defer teardown()

	d := wireDispatcher(t, emitter)
	defer d.Close(context.Background())

	jobID := fmt.Sprintf("fail-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:3.20",
		// Sleep first so the informer sees the Running state before termination,
		// then exit non-zero. Otherwise the watcher's "already-terminated on
		// first sight" path (intended for leader failover) skips emission.
		Command:        "sleep 1 && echo 'about to fail' && exit 2",
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Callback:       &job.Callback{URL: callbackURL},
	}
	if err := o.Run(t.Context(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		s, err := o.Status(t.Context(), jobID)
		return err == nil && s.State == job.StateFailed
	}, testutil.WithTimeout(120*time.Second), testutil.WithInterval(time.Second))

	status, err := o.Status(t.Context(), jobID)
	if err != nil {
		t.Fatalf("final Status: %v", err)
	}
	if status.State != job.StateFailed {
		t.Errorf("state: want failed, got %s", status.State)
	}
	if status.ExitCode == nil || *status.ExitCode != 2 {
		t.Errorf("exit code: want 2, got %v", status.ExitCode)
	}

	testutil.MustWaitFor(t, func() bool {
		return events.has(job.CallbackTypeStart) && events.has(job.CallbackTypeExit)
	}, testutil.WithTimeout(10*time.Second))
}

// --- stop ---

// TestIntegration_Stop starts a long-running job, stops it, and verifies the
// underlying K8s Job is deleted and Status reports NotFound.
func TestIntegration_Stop(t *testing.T) {
	_, _, closeCallback := startCallbackServer(t)
	defer closeCallback()

	o, _, teardown := setup(t)
	defer teardown()

	jobID := fmt.Sprintf("stop-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:3.20",
		Command:        "sleep 600",
		TimeoutSeconds: 900,
		Workspace:      "/workspace",
	}
	if err := o.Run(t.Context(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait until the Job is at least Accepted (created in K8s).
	testutil.MustWaitFor(t, func() bool {
		_, err := o.Status(t.Context(), jobID)
		return err == nil
	}, testutil.WithTimeout(30*time.Second))

	if err := o.Stop(t.Context(), jobID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		_, err := o.Status(t.Context(), jobID)
		return err != nil
	}, testutil.WithTimeout(30*time.Second))
}

// --- helpers ---

// callbackServer captures received CloudEvent payloads and indexes them by type.
type callbackServer struct {
	mu     sync.Mutex
	byType map[string]int
}

func (c *callbackServer) has(eventType string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byType[eventType] > 0
}

func startCallbackServer(t *testing.T) (*callbackServer, string, func()) {
	t.Helper()
	c := &callbackServer{byType: make(map[string]int)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.ReadAll(r.Body)
		eventType := r.Header.Get("Ce-Type")
		c.mu.Lock()
		c.byType[eventType]++
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return c, srv.URL, srv.Close
}

// wireDispatcher registers a dispatcher on the supplied emitter so callback
// envelopes actually POST out to the test's callback server.
func wireDispatcher(t *testing.T, emitter *job.CallbackEmitter) *dispatcher.Memory {
	t.Helper()
	d := dispatcher.NewMemory(dispatcher.Config{BufferSize: 100, Workers: 4}, nil)
	emitter.Register(func(e *job.CallbackEnvelope) {
		if e.CallbackURL == "" {
			return
		}
		_ = d.Dispatch(&dispatcher.Event{
			Payload:     e.Payload,
			Destination: e.CallbackURL,
			SigningKey:  e.SigningKey,
		})
	})
	return d
}
