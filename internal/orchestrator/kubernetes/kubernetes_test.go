package kubernetes

import (
	"context"
	"orchestrator/pkg/job"
	"orchestrator/internal/testutil"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// scriptedWatcher emits a fixed signal sequence for every Watch call and returns.
// Each call is independent, so the same instance is safe to reuse across jobs.
type scriptedWatcher struct {
	signals []job.Signal
}

func newScriptedWatcher(signals ...job.Signal) *scriptedWatcher {
	return &scriptedWatcher{signals: signals}
}

func (s *scriptedWatcher) Watch(ctx context.Context, _, _ string, emit func(job.Signal)) {
	for _, sig := range s.signals {
		if ctx.Err() != nil {
			return
		}
		emit(sig)
	}
}

// blockingWatcher blocks each Watch call on ctx.Done. Used when a test does not
// care about signals but does care that the watcher is alive while the job lives.
type blockingWatcher struct{}

func (blockingWatcher) Watch(ctx context.Context, _, _ string, _ func(job.Signal)) {
	<-ctx.Done()
}

func newTestOrchestrator(t *testing.T, watcher LifecycleWatcher) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	cfg := OrchestratorConfig{
		Namespace:                     "orchestrator",
		ServiceAccount:                "job-sidecar",
		JobRetention:                  15 * time.Minute,
		MaintenanceInterval:           1 * time.Minute,
		ArtifactEndpoint:              "http://jobs-service.orchestrator.svc:8080",
		TerminationGracePeriodSeconds: 600,
	}
	o := &Orchestrator{
		client:              cs,
		namespace:           cfg.Namespace,
		sidecarImage:        "sidecar:latest",
		cfg:                 cfg,
		retentionPeriod:     cfg.JobRetention,
		maintenanceInterval: cfg.MaintenanceInterval,
		emitter:             job.NewCallbackEmitter(),
		ctrl:                job.NewMemoryStore[kubernetesHandle](),
		watcher:             watcher,
	}
	return o, cs
}

// --- Run / Stop ---

func TestRun_CreatesKubernetesJob(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t, blockingWatcher{})
	defer o.Close()

	req := &job.Request{
		ID:             "job-1",
		Image:          "alpine:latest",
		Command:        "echo hi",
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
	}
	if err := o.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := cs.BatchV1().Jobs("orchestrator").Get(context.Background(), "job-job-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected Job to exist: %v", err)
	}
	if got.Labels[LabelJobID] != "job-1" {
		t.Errorf("job.id label: got %s", got.Labels[LabelJobID])
	}
	if got.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("managed-by label: got %s", got.Labels[LabelManagedBy])
	}

	status, err := o.Status(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != job.StateAccepted {
		t.Errorf("state: want accepted, got %s", status.State)
	}
}

func TestRun_DuplicateIDConflict(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, blockingWatcher{})
	defer o.Close()

	req := &job.Request{ID: "dup", Image: "alpine:latest"}
	if err := o.Run(context.Background(), req); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := o.Run(context.Background(), req); err == nil {
		t.Error("expected conflict on second Run, got nil")
	}
}

func TestStop_DeletesJobAndReleases(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t, blockingWatcher{})
	defer o.Close()

	req := &job.Request{ID: "stop-me", Image: "alpine:latest"}
	if err := o.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := o.Stop(context.Background(), "stop-me"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	_, err := cs.BatchV1().Jobs("orchestrator").Get(context.Background(), "job-stop-me", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected Job deleted, got err=%v", err)
	}
	if _, err := o.Status(context.Background(), "stop-me"); err == nil {
		t.Error("expected not-found status after Stop")
	}
}

func TestStop_UnknownJob(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, blockingWatcher{})
	defer o.Close()

	if err := o.Stop(context.Background(), "ghost"); err == nil {
		t.Error("expected not-found error")
	}
}

// --- Signal application through the store ---

func TestRun_AppliesStartedAndExited(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, newScriptedWatcher(
		job.Started{},
		job.Exited{ExitCode: 0, Duration: time.Second},
	))
	defer o.Close()

	if err := o.Run(context.Background(), &job.Request{ID: "flow", Image: "alpine:latest"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		status, err := o.Status(context.Background(), "flow")
		return err == nil && status.State == job.StateCompleted
	}, testutil.WithTimeout(5*time.Second))

	status, _ := o.Status(context.Background(), "flow")
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Errorf("exit code: got %v", status.ExitCode)
	}
}

func TestRun_FailedBeforeStart(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, newScriptedWatcher(
		job.Failed{Reason: "image pull"},
	))
	defer o.Close()

	if err := o.Run(context.Background(), &job.Request{ID: "fail-early", Image: "alpine:latest"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		status, err := o.Status(context.Background(), "fail-early")
		return err == nil && status.State == job.StateFailed
	}, testutil.WithTimeout(5*time.Second))

	status, _ := o.Status(context.Background(), "fail-early")
	if status.Error != "image pull" {
		t.Errorf("error: got %q", status.Error)
	}
}

// --- Reconciliation ---

func TestReconcile_ResumesRunning(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset(
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "job-resume-1",
				Namespace: "orchestrator",
				Labels: map[string]string{
					LabelManagedBy: ManagedByValue,
					LabelJobID:     "resume-1",
				},
			},
			Status: batchv1.JobStatus{Active: 1},
		},
	)
	o := &Orchestrator{
		client:          cs,
		namespace:       "orchestrator",
		cfg:             OrchestratorConfig{Namespace: "orchestrator"},
		retentionPeriod: 15 * time.Minute,
		emitter:         job.NewCallbackEmitter(),
		ctrl:            job.NewMemoryStore[kubernetesHandle](),
		watcher:         newScriptedWatcher(job.Started{}),
	}
	defer o.Close()

	if err := o.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		status, err := o.Status(context.Background(), "resume-1")
		return err == nil && status.State == job.StateRunning
	}, testutil.WithTimeout(5*time.Second))
}

func TestReconcile_MarksTerminalCompleted(t *testing.T) {
	t.Parallel()
	o := reconcileHarness(t, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-done-1",
			Namespace: "orchestrator",
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelJobID:     "done-1",
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	})
	defer o.Close()

	if err := o.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	status, err := o.Status(context.Background(), "done-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != job.StateCompleted {
		t.Errorf("state: want completed, got %s", status.State)
	}
}

func TestReconcile_MarksTerminalFailed(t *testing.T) {
	t.Parallel()
	o := reconcileHarness(t, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-bad-1",
			Namespace: "orchestrator",
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelJobID:     "bad-1",
			},
		},
		Status: batchv1.JobStatus{Failed: 1},
	})
	defer o.Close()

	if err := o.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	status, err := o.Status(context.Background(), "bad-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != job.StateFailed {
		t.Errorf("state: want failed, got %s", status.State)
	}
}

func reconcileHarness(t *testing.T, seed *batchv1.Job) *Orchestrator {
	t.Helper()
	cs := fake.NewClientset(seed)
	return &Orchestrator{
		client:          cs,
		namespace:       "orchestrator",
		cfg:             OrchestratorConfig{Namespace: "orchestrator"},
		retentionPeriod: 15 * time.Minute,
		emitter:         job.NewCallbackEmitter(),
		ctrl:            job.NewMemoryStore[kubernetesHandle](),
		watcher:         blockingWatcher{},
	}
}

// --- List / Ready ---

func TestList_ReturnsAllEntries(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, blockingWatcher{})
	defer o.Close()

	for _, id := range []string{"a", "b", "c"} {
		if err := o.Run(context.Background(), &job.Request{ID: id, Image: "alpine:latest"}); err != nil {
			t.Fatalf("Run(%s): %v", id, err)
		}
	}

	list, err := o.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("List len: want 3, got %d", len(list))
	}
}

func TestReady_ContactsAPIServer(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, blockingWatcher{})
	defer o.Close()
	if err := o.Ready(context.Background()); err != nil {
		t.Errorf("Ready: %v", err)
	}
}
