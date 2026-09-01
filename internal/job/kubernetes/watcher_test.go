package kubernetes

import (
	"fmt"
	"strings"
	"testing"

	"orchestrator/internal/job"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// initFailedPod models what kubelet actually reports when an init container
// fails: pod Failed, the failing init container terminated non-zero, and the
// worker present in containerStatuses but stuck waiting/PodInitializing —
// it exists, it just never ran.
func initFailedPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "job-x-abc", Labels: map[string]string{LabelJobID: "x"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  "artifact-pre",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  ContainerWorker,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
			}},
		},
	}
}

// TestTracker_InitFailureBeforeWorkerRan pins the fixed loss mode: a pod that
// fails during init (worker listed but never started) must emit Failed and
// reach terminal state on first observation — previously it matched no branch
// at all and the job sat untracked until pod retention deleted it.
func TestTracker_InitFailureBeforeWorkerRan(t *testing.T) {
	em := job.NewCallbackEmitter()
	var events []*job.CallbackEnvelope
	em.Register(func(env *job.CallbackEnvelope) { events = append(events, env) })

	w := newK8sLifecycleWatcher(fake.NewClientset(), "ns", em, 0)
	tr := newJobTracker(w, &watchConfig{jobID: "x", dest: &job.CallbackDest{URL: "http://cb"}})

	tr.mu.Lock()
	terminal := tr.applyPodStateLocked(t.Context(), initFailedPod())
	tr.mu.Unlock()

	if !terminal {
		t.Fatal("init-failed pod must be terminal on first observation")
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly one callback, got %d", len(events))
	}
	payload := fmt.Sprintf("%v", events[0].Payload.Data)
	if !strings.Contains(payload, "init container artifact-pre failed with exit code 1") {
		t.Fatalf("callback should name the failing init container, got %s", payload)
	}
}

// TestTracker_WorkerRunningStillStarts guards the fix against over-matching:
// a healthy pod whose worker is running must take the normal Started path.
func TestTracker_WorkerRunningStillStarts(t *testing.T) {
	em := job.NewCallbackEmitter()
	var events []*job.CallbackEnvelope
	em.Register(func(env *job.CallbackEnvelope) { events = append(events, env) })

	pod := initFailedPod()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.InitContainerStatuses = nil
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}

	w := newK8sLifecycleWatcher(fake.NewClientset(), "ns", em, 0)
	tr := newJobTracker(w, &watchConfig{jobID: "x", dest: &job.CallbackDest{URL: "http://cb"}})

	tr.mu.Lock()
	terminal := tr.applyPodStateLocked(t.Context(), pod)
	tr.mu.Unlock()

	if terminal {
		t.Fatal("running pod must not be terminal")
	}
	if len(events) != 1 {
		t.Fatalf("expected one callback, got %d", len(events))
	}
	if events[0].Payload.Type != job.CallbackTypeStart {
		t.Fatalf("expected start event, got %s", events[0].Payload.Type)
	}
}
