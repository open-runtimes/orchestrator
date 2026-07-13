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

func TestJobTracker_PodDeletedAfterWorkerExit_EmitsComplete(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	tr := newJobTracker(w, &watchConfig{
		jobID: "force-deleted-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	tr.handleUpdate(t.Context(), podWithWorkerRunning())
	tr.handleUpdate(t.Context(), podWithWorkerTerminated(0, corev1.PodRunning))
	capture.assertHasType(t, job.CallbackTypeExit)

	// Pod force-deleted before a terminal phase is observed: complete must
	// still fire so consumers waiting on it aren't left hanging.
	capture.reset()
	tr.handleDelete()
	capture.assertHasType(t, job.CallbackTypeComplete)
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
	tr.handleUpdate(t.Context(), podWithWorkerTerminated(0, corev1.PodSucceeded))
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

	// Worker exits while the artifact sidecar is still processing post-job
	// artifacts: exit fires, complete does not.
	capture.reset()
	tr.handleUpdate(t.Context(), podWithWorkerTerminated(0, corev1.PodRunning))
	capture.assertHasType(t, job.CallbackTypeExit)
	if slices.Contains(capture.types(), job.CallbackTypeComplete) {
		t.Errorf("complete must not fire before the pod terminates, got %v", capture.types())
	}

	// Sidecar finishes and the pod reaches a terminal phase: complete fires.
	capture.reset()
	tr.handleUpdate(t.Context(), podWithWorkerTerminated(0, corev1.PodSucceeded))
	capture.assertHasType(t, job.CallbackTypeComplete)
}

// --- helpers ---

type eventCapture struct {
	mu     sync.Mutex
	events []string
}

func (c *eventCapture) register(emitter *job.CallbackEmitter) {
	emitter.Register(func(e *job.CallbackEnvelope) {
		if e.Payload == nil {
			return
		}
		c.mu.Lock()
		c.events = append(c.events, e.Payload.Type)
		c.mu.Unlock()
	})
}

func (c *eventCapture) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	copy(out, c.events)
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
	w := newK8sLifecycleWatcher(fake.NewClientset(), "test", emitter, nil, 0)
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

func podWithWorkerTerminated(exitCode int32, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "test"},
		Status: corev1.PodStatus{
			Phase: phase,
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
