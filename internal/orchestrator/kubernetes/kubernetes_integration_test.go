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
	"orchestrator/internal/artifact"
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
func setup(t *testing.T, opts ...func(*Config)) (*Orchestrator, *job.CallbackEmitter, func()) {
	t.Helper()
	ctx := t.Context()

	emitter := job.NewCallbackEmitter()
	cfg := Config{
		SidecarImage: sidecarImage,
		// Pin the kubeconfig context explicitly: this test must only ever run
		// against the project's kind cluster, never the user's current-context.
		Context:                "kind-orchestrator-dev",
		Namespace:              testNamespace,
		SidecarImagePullPolicy: "Never", // ko-loaded image lives locally in kind
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	factory := NewOrchestrator(ctx, cfg)
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

	// closeOnly tears down the Orchestrator without touching K8s Jobs. Use
	// when another Orchestrator takes over on the same cluster state (e.g.
	// rolling deployment test).
	closeOnly := func() { orch.Close() }
	// teardown additionally deletes all managed Jobs (and their child Pods
	// via background propagation) so successive test runs don't pollute
	// each other.
	teardown := func() {
		prop := metav1.DeletePropagationBackground
		_ = o.client.BatchV1().Jobs(testNamespace).DeleteCollection(
			context.Background(),
			metav1.DeleteOptions{PropagationPolicy: &prop},
			metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue},
		)
		orch.Close()
	}
	_ = closeOnly // exposed via setupNoCleanup below
	return o, emitter, teardown
}

// setupNoCleanup is like setup but returns a teardown that does NOT delete
// Jobs, for tests that simulate multi-orchestrator takeover against shared
// cluster state.
func setupNoCleanup(t *testing.T) (*Orchestrator, *job.CallbackEmitter, func()) {
	t.Helper()
	o, emitter, teardown := setup(t)
	_ = teardown
	closeOnly := func() { o.Close() }
	return o, emitter, closeOnly
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

// --- squashfs mount ---

// TestIntegration_SquashfsMount builds a squashfs image in the workspace
// (write → archive), mounts it read-only via a `mount` artifact, and has the
// worker read a file back out of the mount. Reaching Completed proves the
// privileged post sidecar mounted the image, propagation reached the worker,
// and the contents round-tripped. Requires the squashfs kernel module on nodes.
func TestIntegration_SquashfsMount(t *testing.T) {
	o, emitter, teardown := setup(t)
	defer teardown()

	d := wireDispatcher(t, emitter)
	defer d.Close(context.Background())

	jobID := fmt.Sprintf("mount-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:    jobID,
		Image: "alpine:3.20",
		// Fails (non-zero) unless the mounted file is present with the right
		// content — so Completed is a real assertion that the mount worked.
		Command:        `sleep 1 && grep -q "mounted content" /workspace/mnt/hello.txt`,
		CPU:            0.1,
		Memory:         64,
		TimeoutSeconds: 120,
		Workspace:      "/workspace",
		Artifacts: []artifact.Artifact{
			&artifact.Write{ID: "w", In: "mounted content", Out: "hello.txt"},
			&artifact.Archive{ID: "a", In: "hello.txt", Out: "data.sqfs", Format: "squashfs", Depends: "w"},
			&artifact.Mount{ID: "m", In: "data.sqfs", Out: "mnt", Depends: "a"},
		},
	}
	if err := o.Run(t.Context(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		s, err := o.Status(t.Context(), jobID)
		return err == nil && (s.State == job.StateCompleted || s.State == job.StateFailed)
	}, testutil.WithTimeout(150*time.Second), testutil.WithInterval(time.Second))

	status, err := o.Status(t.Context(), jobID)
	if err != nil {
		t.Fatalf("final Status: %v", err)
	}
	if status.State != job.StateCompleted {
		t.Errorf("final state: want completed (mount + read succeeded), got %s (exit=%v)", status.State, status.ExitCode)
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

// --- rolling deployment ---

// TestIntegration_RollingHandoff simulates a rolling orchestrator deploy: o1
// starts and begins a long-running job, o1 graceful-shutdowns mid-job, a fresh
// o2 starts against the same namespace, and we verify the Exit callback still
// lands (emitted by o2's watcher after its informer picks up the in-flight Pod).
func TestIntegration_RollingHandoff(t *testing.T) {
	events, callbackURL, closeCallback := startCallbackServer(t)
	defer closeCallback()

	o1, emitter1, closeO1 := setupNoCleanup(t)
	d1 := wireDispatcher(t, emitter1)

	jobID := fmt.Sprintf("rolling-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:3.20",
		Command:        "echo phase-1 && sleep 10 && echo phase-2",
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Callback:       &job.Callback{URL: callbackURL},
	}
	if err := o1.Run(t.Context(), req); err != nil {
		t.Fatalf("o1 Run: %v", err)
	}

	// Wait for Running on o1.
	testutil.MustWaitFor(t, func() bool {
		s, err := o1.Status(t.Context(), jobID)
		return err == nil && s.State == job.StateRunning
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(500*time.Millisecond))

	testutil.MustWaitFor(t, func() bool {
		return events.has(job.CallbackTypeStart)
	}, testutil.WithTimeout(5*time.Second))

	// Graceful shutdown of o1 — leaves the Job behind.
	closeO1()
	d1.Close(context.Background())

	// Fresh orchestrator takes over. teardown2 deletes the Job at the end.
	o2, emitter2, teardown2 := setup(t)
	defer teardown2()
	d2 := wireDispatcher(t, emitter2)
	defer d2.Close(context.Background())

	// Job should complete; o2's informer sees the in-flight Pod and picks up
	// where the state machine left off.
	testutil.MustWaitFor(t, func() bool {
		s, err := o2.Status(t.Context(), jobID)
		return err == nil && (s.State == job.StateCompleted || s.State == job.StateFailed)
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	s, err := o2.Status(t.Context(), jobID)
	if err != nil {
		t.Fatalf("final Status: %v", err)
	}
	if s.State != job.StateCompleted {
		t.Errorf("state: want completed, got %s", s.State)
	}

	// Exit callback must land on the server, emitted by o2.
	testutil.MustWaitFor(t, func() bool {
		return events.has(job.CallbackTypeExit)
	}, testutil.WithTimeout(10*time.Second))
}

// --- mid-job leader failover ---

// TestIntegration_LeaderFailoverMidJob runs two orchestrators with leader
// election enabled, sharing the cluster. The leader runs a job; we kill the
// leader mid-flight and verify the surviving replica takes over and delivers
// the Exit callback when the job finishes.
func TestIntegration_LeaderFailoverMidJob(t *testing.T) {
	events, callbackURL, closeCallback := startCallbackServer(t)
	defer closeCallback()

	emitter := job.NewCallbackEmitter()
	d := wireDispatcher(t, emitter)
	defer d.Close(context.Background())

	mkOrch := func(id string) *Orchestrator {
		factory := NewOrchestrator(t.Context(), Config{
			SidecarImage:           sidecarImage,
			Context:                "kind-orchestrator-dev",
			Namespace:              testNamespace,
			SidecarImagePullPolicy: "Never",
			LeaderElection: LeaderElectionConfig{
				Enabled:       true,
				LeaseName:     "test-handoff-lease",
				Identity:      id,
				LeaseDuration: 2 * time.Second,
				RenewDeadline: 1 * time.Second,
				RetryPeriod:   200 * time.Millisecond,
			},
		})
		orch, err := factory(emitter)
		if err != nil {
			t.Fatalf("NewOrchestrator(%s): %v", id, err)
		}
		return orch.(*Orchestrator)
	}

	o1 := mkOrch("replica-1")
	o2 := mkOrch("replica-2")

	// Ensure namespace + job-sidecar SA exist (normally setup() handles it).
	ctx := t.Context()
	_, err := o1.client.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, _ = o1.client.CoreV1().Namespaces().Create(ctx, nsObject(testNamespace), metav1.CreateOptions{})
	}
	_, err = o1.client.CoreV1().ServiceAccounts(testNamespace).Get(ctx, "job-sidecar", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, _ = o1.client.CoreV1().ServiceAccounts(testNamespace).Create(ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "job-sidecar", Namespace: testNamespace},
		}, metav1.CreateOptions{})
	}

	if err := o1.Start(ctx); err != nil {
		t.Fatalf("o1.Start: %v", err)
	}
	if err := o2.Start(ctx); err != nil {
		t.Fatalf("o2.Start: %v", err)
	}
	defer func() {
		o1.Close()
		o2.Close()
		prop := metav1.DeletePropagationBackground
		_ = o1.client.BatchV1().Jobs(testNamespace).DeleteCollection(
			context.Background(),
			metav1.DeleteOptions{PropagationPolicy: &prop},
			metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue},
		)
	}()

	// Wait until the lease has been claimed so we know a leader is live.
	testutil.MustWaitFor(t, func() bool {
		lease, err := o1.client.CoordinationV1().Leases(testNamespace).Get(ctx, "test-handoff-lease", metav1.GetOptions{})
		return err == nil && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != ""
	}, testutil.WithTimeout(10*time.Second))

	lease, _ := o1.client.CoordinationV1().Leases(testNamespace).Get(ctx, "test-handoff-lease", metav1.GetOptions{})
	leaderID := *lease.Spec.HolderIdentity
	t.Logf("initial leader: %s", leaderID)

	// Either replica can accept Run (it's stateless). Use o1.
	jobID := fmt.Sprintf("failover-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:3.20",
		Command:        "echo phase-1 && sleep 8 && echo phase-2",
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Callback:       &job.Callback{URL: callbackURL},
	}
	if err := o1.Run(ctx, req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait for Started — confirms the current leader's watcher is emitting.
	testutil.MustWaitFor(t, func() bool {
		return events.has(job.CallbackTypeStart)
	}, testutil.WithTimeout(30*time.Second))

	// Kill the leader. Close releases the lease (ReleaseOnCancel=true).
	if leaderID == "replica-1" {
		o1.Close()
	} else {
		o2.Close()
	}

	// Wait for lease to transfer.
	testutil.MustWaitFor(t, func() bool {
		lease, err := o2.client.CoordinationV1().Leases(testNamespace).Get(ctx, "test-handoff-lease", metav1.GetOptions{})
		if err != nil {
			return false
		}
		if lease.Spec.HolderIdentity == nil {
			return false
		}
		return *lease.Spec.HolderIdentity != leaderID
	}, testutil.WithTimeout(15*time.Second))

	// The surviving replica must observe the job completing and emit Exit.
	testutil.MustWaitFor(t, func() bool {
		return events.has(job.CallbackTypeExit)
	}, testutil.WithTimeout(30*time.Second))
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
