package kubernetes

import (
	"orchestrator/internal/job"
	"slices"
	"sync"
	"testing"
	"time"

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
	w.termStart = time.Now()

	tr := newJobTracker(w, &watchConfig{
		jobID: "takeover-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	// Pod created before this leadership term, already Terminated on first
	// sight — assume the previous leader emitted. No callbacks should fire.
	pod := podWithWorkerTerminated(0, corev1.PodSucceeded)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	tr.handleUpdate(t.Context(), pod)
	if len(capture.types()) != 0 {
		t.Errorf("expected no callbacks for already-terminated pod on first sight, got %v", capture.types())
	}
}

func TestJobTracker_FastJobFirstSeenTerminal_EmitsFullLifecycle(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)
	w.termStart = time.Now().Add(-time.Minute)

	tr := newJobTracker(w, &watchConfig{
		jobID: "fast-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	// Pod created during this leadership term but first observed with the
	// worker already terminated (fast job, informer coalesced the running
	// state away). No previous leader can have emitted anything — the full
	// lifecycle must be synthesized from the terminal container status.
	pod := podWithWorkerTerminated(0, corev1.PodSucceeded)
	pod.CreationTimestamp = metav1.Now()
	tr.handleUpdate(t.Context(), pod)

	capture.assertHasType(t, job.CallbackTypeStart)
	capture.assertHasType(t, job.CallbackTypeExit)
	capture.assertHasType(t, job.CallbackTypeComplete)
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

func TestJobTracker_OOMKilledWorker_ExitCarriesReason(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	var reasons []any
	w.emitter.Register(func(e *job.CallbackEnvelope) {
		if e.Payload != nil && e.Payload.Type == job.CallbackTypeExit {
			reasons = append(reasons, e.Payload.Data["reason"])
		}
	})

	tr := newJobTracker(w, &watchConfig{
		jobID: "oom-job",
		image: "alpine:latest",
		dest:  &job.CallbackDest{URL: "https://cb.example"},
	})

	tr.handleUpdate(t.Context(), podWithWorkerRunning())
	pod := podWithWorkerTerminated(137, corev1.PodRunning)
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled"
	tr.handleUpdate(t.Context(), pod)

	capture.assertHasType(t, job.CallbackTypeExit)
	if len(reasons) != 1 || reasons[0] != job.ExitReasonOOM {
		t.Errorf("want exit reason %q, got %v", job.ExitReasonOOM, reasons)
	}
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
	w := newK8sLifecycleWatcher(fake.NewClientset(), "test", emitter, 0)
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

// Counts backs the jobs_active and orchestrator_trackers async gauges. A
// tracker stays in the map after its worker exits (until the pod is deleted),
// so it keeps counting as a tracker but stops counting as an active job.
func TestWatcher_Counts(t *testing.T) {
	t.Parallel()
	_, w := newTrackerFixture(t)

	pod := podWithWorkerRunning()
	pod.Labels = map[string]string{LabelJobID: "counted-job"}
	w.handle(t.Context(), pod, false)

	if trackers, active := w.Counts(); trackers != 1 || active != 1 {
		t.Fatalf("running: want 1 tracker / 1 active, got %d / %d", trackers, active)
	}

	exited := podWithWorkerTerminated(0, corev1.PodRunning)
	exited.Labels = map[string]string{LabelJobID: "counted-job"}
	w.handle(t.Context(), exited, false)

	if trackers, active := w.Counts(); trackers != 1 || active != 0 {
		t.Fatalf("exited: want 1 tracker / 0 active, got %d / %d", trackers, active)
	}

	w.handle(t.Context(), exited, true)

	if trackers, active := w.Counts(); trackers != 0 || active != 0 {
		t.Fatalf("deleted: want 0 tracker / 0 active, got %d / %d", trackers, active)
	}
}

// A pod that fails before its worker ever starts reaches terminal state without
// setting isExited, and its tracker stays mapped until the pod is deleted — a
// retention period away, or forever if TTL cleanup never runs. It must stop
// counting as active the moment it goes terminal.
func TestWatcher_Counts_PodFailedBeforeWorkerStarted(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "test",
			Labels:    map[string]string{LabelJobID: "never-started-job"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "ImagePullBackOff"},
	}
	w.handle(t.Context(), pod, false)
	capture.assertHasType(t, job.CallbackTypeExit)

	if trackers, active := w.Counts(); trackers != 1 || active != 0 {
		t.Fatalf("want 1 tracker / 0 active, got %d / %d", trackers, active)
	}
}

// podInitFailedWorkerWaiting models what kubelet actually reports when an init
// container fails: pod Failed, the failing init container terminated non-zero,
// and the worker PRESENT in containerStatuses but stuck waiting/PodInitializing
// — it exists, it just never ran. (TestWatcher_Counts_PodFailedBeforeWorkerStarted
// above models a pod with no container statuses at all; kubelet rarely reports
// that shape, which is how the untracked-job bug survived its coverage.)
func podInitFailedWorkerWaiting() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "test"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "artifact-pre",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
					},
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: ContainerWorker,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
					},
				},
			},
		},
	}
}

// TestJobTracker_InitFailureBeforeWorkerRan pins the fixed loss mode: a pod
// that fails during init must emit a Failed callback naming the culprit and
// reach terminal state on first observation — previously it matched no branch
// at all, and the job sat untracked (no callback, no log) until pod retention
// deleted it ~15 minutes later.
func TestJobTracker_InitFailureBeforeWorkerRan(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	var reasons []any
	w.emitter.Register(func(e *job.CallbackEnvelope) {
		if e.Payload != nil && e.Payload.Type == job.CallbackTypeExit {
			reasons = append(reasons, e.Payload.Data["reason"])
		}
	})

	pod := podInitFailedWorkerWaiting()
	pod.Labels = map[string]string{LabelJobID: "init-failed-job"}
	w.handle(t.Context(), pod, false)

	capture.assertHasType(t, job.CallbackTypeExit)
	want := "init container artifact-pre failed with exit code 1"
	if len(reasons) != 1 || reasons[0] != want {
		t.Errorf("want reason %q, got %v", want, reasons)
	}
	if trackers, active := w.Counts(); trackers != 1 || active != 0 {
		t.Fatalf("want 1 tracker / 0 active, got %d / %d", trackers, active)
	}
}

// TestJobTracker_InitFailureShapeDoesNotOvermatch guards the state-check fix:
// a healthy pod whose worker is running takes the normal Started path even
// with init container statuses present.
func TestJobTracker_InitFailureShapeDoesNotOvermatch(t *testing.T) {
	t.Parallel()
	capture, w := newTrackerFixture(t)

	pod := podWithWorkerRunning()
	pod.Labels = map[string]string{LabelJobID: "healthy-job"}
	pod.Annotations = map[string]string{AnnotationCallbackURL: "https://cb.example"}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "artifact-pre", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
	}
	w.handle(t.Context(), pod, false)

	capture.assertHasType(t, job.CallbackTypeStart)
	if trackers, active := w.Counts(); trackers != 1 || active != 1 {
		t.Fatalf("want 1 tracker / 1 active, got %d / %d", trackers, active)
	}
}

func TestJobTracker_GatedStartup(t *testing.T) {
	capture, w := newTrackerFixture(t)
	tr := newJobTracker(w, &watchConfig{jobID: "gated", dest: &job.CallbackDest{URL: "https://cb.example"}})
	defer tr.close()
	pod := podWithWorkerRunning()
	pod.Annotations = map[string]string{annotationStartupGate: startupGateVersion}
	tr.handleUpdate(t.Context(), pod)
	if len(capture.types()) != 0 {
		t.Fatal("waiting shell must not emit start")
	}
	started := true
	pod.Status.ContainerStatuses[0].Started = &started
	tr.handleUpdate(t.Context(), pod)
	capture.assertHasType(t, job.CallbackTypeStart)
	// Node loss may remove the probe observation. Remember an already
	// observed execution instead of parking the tracker behind the gate.
	capture.reset()
	pod.Status.ContainerStatuses[0].Started = nil
	pod.Status.Phase = corev1.PodFailed
	tr.handleUpdate(t.Context(), pod)
	capture.assertHasType(t, job.CallbackTypeExit)
}

func TestJobTracker_GatedTermination(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		wantStart     bool
	}{
		{name: "setup failed"},
		{name: "fast command", message: "orchestrator-started:1700000000", wantStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture, w := newTrackerFixture(t)
			w.termStart = time.Now()
			tr := newJobTracker(w, &watchConfig{jobID: "gated", dest: &job.CallbackDest{URL: "https://cb.example"}})
			defer tr.close()
			pod := podWithWorkerTerminated(1, corev1.PodFailed)
			pod.CreationTimestamp = metav1.NewTime(w.termStart.Truncate(time.Second))
			pod.Annotations = map[string]string{annotationStartupGate: startupGateVersion}
			pod.Status.ContainerStatuses[0].State.Terminated.Message = tc.message
			tr.handleUpdate(t.Context(), pod)
			if slices.Contains(capture.types(), job.CallbackTypeStart) != tc.wantStart {
				t.Fatalf("incorrect start emission: %v", capture.types())
			}
			capture.assertHasType(t, job.CallbackTypeExit)
		})
	}
}
