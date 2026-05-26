package kubernetes

import (
	"orchestrator/pkg/job"
	"slices"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// jobTracker unit tests — drive applyPodState directly with crafted Pod
// objects to pin down each edge of the state machine without needing an
// informer.

func TestJobTracker_NodeLostAfterStart(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	tr := newJobTracker(w, &watchConfig{
		jobID: "node-lost-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	// First, pod reaches Running — should emit Started.
	tr.handleUpdate(t.Context(), podWithWorkerRunning())
	capture.assertHasType(t, job.CallbackTypeStart)

	// Then the node is lost: pod.Phase=Failed, worker container status may
	// still show Running (stale) — we must still emit Failed (mapped to
	// CallbackTypeExit with exit_code=-1 by EmitCallback).
	capture.reset()
	tr.handleUpdate(t.Context(), podNodeLostWithStaleRunning())
	capture.assertHasType(t, job.CallbackTypeExit)
}

func TestJobTracker_PodDeletedMidJob(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	tr := newJobTracker(w, &watchConfig{
		jobID: "deleted-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	tr.handleUpdate(t.Context(), podWithWorkerRunning())
	capture.assertHasType(t, job.CallbackTypeStart)

	capture.reset()
	tr.handleDelete()
	capture.assertHasType(t, job.CallbackTypeExit)
}

func TestJobTracker_AlreadyTerminatedSkipsEmission(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	tr := newJobTracker(w, &watchConfig{
		jobID: "takeover-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	// New tracker, pod already Terminated — assume a previous leader emitted.
	// No callbacks should fire.
	tr.handleUpdate(t.Context(), podWithWorkerTerminated(0))
	if len(capture.types()) != 0 {
		t.Errorf("expected no callbacks for already-terminated pod on first sight, got %v", capture.types())
	}
}

func TestJobTracker_HappyPath(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	tr := newJobTracker(w, &watchConfig{
		jobID: "happy-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	tr.handleUpdate(t.Context(), podWithWorkerRunning())
	capture.assertHasType(t, job.CallbackTypeStart)

	capture.reset()
	tr.handleUpdate(t.Context(), podWithWorkerTerminated(0))
	capture.assertHasType(t, job.CallbackTypeExit)
}

func TestJobTracker_LogBatchSequencesIncreaseFromOne(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	tr := newJobTracker(w, &watchConfig{
		jobID: "logs-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	tr.emitLogBatch("stdout", []string{"one"})
	tr.emitLogBatch("stdout", []string{"two"})

	events := capture.all()
	if len(events) != 2 {
		t.Fatalf("want 2 log events, got %d", len(events))
	}
	if events[0].Payload.Data["sequence"] != uint64(1) || events[1].Payload.Data["sequence"] != uint64(2) {
		t.Fatalf("want sequences [1 2], got [%v %v]", events[0].Payload.Data["sequence"], events[1].Payload.Data["sequence"])
	}
	if final := tr.finalLogSequenceLocked(); final != 2 {
		t.Fatalf("want final sequence 2, got %d", final)
	}
}

// --- helpers ---

type eventCapture struct {
	mu     sync.Mutex
	events []*job.CallbackEnvelope
}

func (c *eventCapture) register(emitter *job.CallbackEmitter) {
	emitter.Register(func(e *job.CallbackEnvelope) {
		if e.Payload == nil {
			return
		}
		c.mu.Lock()
		c.events = append(c.events, e)
		c.mu.Unlock()
	})
}

func (c *eventCapture) all() []*job.CallbackEnvelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*job.CallbackEnvelope, len(c.events))
	copy(out, c.events)
	return out
}

func (c *eventCapture) types() []string {
	events := c.all()
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Payload.Type
	}
	return out
}

func (c *eventCapture) reset() {
	c.mu.Lock()
	c.events = nil
	c.mu.Unlock()
}

// assertHasType asserts that at least one event of the given type has been
// observed. Tolerates additional log events that stream from the background
// goroutine.
func (c *eventCapture) assertHasType(t *testing.T, want string) {
	t.Helper()
	if slices.Contains(c.types(), want) {
		return
	}
	t.Errorf("expected event of type %q, got %v", want, c.types())
}

func newTrackerFixture(t *testing.T) (*eventCapture, *k8sLifecycleWatcher) {
	t.Helper()
	capture := &eventCapture{}
	emitter := job.NewCallbackEmitter()
	capture.register(emitter)
	w := newK8sLifecycleWatcher(fake.NewClientset(), "test", emitter, nil)
	return capture, w
}

func podWithWorkerRunning() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "test"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: ContainerWorker,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
					},
				},
			},
		},
	}
}

func podWithWorkerTerminated(exitCode int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "test"},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: ContainerWorker,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   exitCode,
							StartedAt:  metav1.Now(),
							FinishedAt: metav1.Now(),
						},
					},
				},
			},
		},
	}
}

// podNodeLostWithStaleRunning mimics a pod whose node has just been lost:
// the pod phase has flipped to Failed with reason NodeLost, but the container
// status still shows Running because kubelet didn't get a chance to update it.
func podNodeLostWithStaleRunning() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "test"},
		Status: corev1.PodStatus{
			Phase:  corev1.PodFailed,
			Reason: "NodeLost",
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: ContainerWorker,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
					},
				},
			},
		},
	}
}
