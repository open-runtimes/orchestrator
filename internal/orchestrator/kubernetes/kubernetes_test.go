package kubernetes

import (
	"context"
	"orchestrator/pkg/job"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// noopWatcher satisfies LifecycleWatcher without doing any work; used by tests
// that exercise the orchestrator's HTTP surface (Run/Stop/Status/List) rather
// than lifecycle events. The watcher's own tests live in watcher_test.go.
type noopWatcher struct{}

func (noopWatcher) Start(ctx context.Context) { <-ctx.Done() }

func newTestOrchestrator(t *testing.T, watcher LifecycleWatcher) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	cfg := OrchestratorConfig{
		Namespace:                     "orchestrator",
		ServiceAccount:                "job-sidecar",
		JobRetention:                  15 * time.Minute,
		ArtifactEndpoint:              "http://jobs-service.orchestrator.svc:8080",
		TerminationGracePeriodSeconds: 600,
	}
	o := &Orchestrator{
		client:       cs,
		namespace:    cfg.Namespace,
		sidecarImage: "sidecar:latest",
		cfg:          cfg,
		emitter:      job.NewCallbackEmitter(),
		watcher:      watcher,
		statusCache:  newStatusCache(),
	}
	return o, cs
}

// --- Run / Stop ---

func TestRun_CreatesKubernetesJob(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t, noopWatcher{})
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
	o, _ := newTestOrchestrator(t, noopWatcher{})
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
	o, cs := newTestOrchestrator(t, noopWatcher{})
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
	o, _ := newTestOrchestrator(t, noopWatcher{})
	defer o.Close()

	if err := o.Stop(context.Background(), "ghost"); err == nil {
		t.Error("expected not-found error")
	}
}

// --- Status derivation against real K8s objects in the fake clientset ---

func TestStatus_DerivesCompleted(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-done",
			Namespace: "orchestrator",
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelJobID:     "done",
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	})
	o := orchestratorWithClient(cs)
	defer o.Close()

	status, err := o.Status(context.Background(), "done")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != job.StateCompleted {
		t.Errorf("state: want completed, got %s", status.State)
	}
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Errorf("exit code: want 0, got %v", status.ExitCode)
	}
}

func TestStatus_DerivesFailedWithPodReason(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset(
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "job-bad",
				Namespace: "orchestrator",
				Labels: map[string]string{
					LabelManagedBy: ManagedByValue,
					LabelJobID:     "bad",
				},
			},
			Status: batchv1.JobStatus{Failed: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "job-bad-xyz",
				Namespace: "orchestrator",
				Labels: map[string]string{
					LabelJobID: "bad",
				},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: ContainerWorker,
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								ExitCode: 137,
								Reason:   "OOMKilled",
							},
						},
					},
				},
			},
		},
	)
	o := orchestratorWithClient(cs)
	defer o.Close()

	status, err := o.Status(context.Background(), "bad")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != job.StateFailed {
		t.Errorf("state: want failed, got %s", status.State)
	}
	if status.ExitCode == nil || *status.ExitCode != 137 {
		t.Errorf("exit code: want 137, got %v", status.ExitCode)
	}
	if status.Error != "OOMKilled" {
		t.Errorf("error: want OOMKilled, got %q", status.Error)
	}
}

func TestStatus_CacheHit(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-cached",
			Namespace: "orchestrator",
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelJobID:     "cached",
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	})
	o := orchestratorWithClient(cs)
	defer o.Close()

	first, err := o.Status(context.Background(), "cached")
	if err != nil {
		t.Fatalf("first Status: %v", err)
	}

	// Delete the underlying Job — a fresh Status would 404, but the cache
	// should still return the prior completed result within the TTL window.
	_ = cs.BatchV1().Jobs("orchestrator").Delete(context.Background(), "job-cached", metav1.DeleteOptions{})

	second, err := o.Status(context.Background(), "cached")
	if err != nil {
		t.Fatalf("second Status: %v", err)
	}
	if first.State != second.State {
		t.Errorf("cache miss: first=%s second=%s", first.State, second.State)
	}
}

// --- List / Ready ---

func TestList_ReturnsAllEntries(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t, noopWatcher{})
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
	o, _ := newTestOrchestrator(t, noopWatcher{})
	defer o.Close()
	if err := o.Ready(context.Background()); err != nil {
		t.Errorf("Ready: %v", err)
	}
}

// --- helpers ---

func orchestratorWithClient(cs *fake.Clientset) *Orchestrator {
	return &Orchestrator{
		client:      cs,
		namespace:   "orchestrator",
		cfg:         OrchestratorConfig{Namespace: "orchestrator"},
		emitter:     job.NewCallbackEmitter(),
		watcher:     noopWatcher{},
		statusCache: newStatusCache(),
	}
}
