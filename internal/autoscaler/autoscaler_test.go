package autoscaler

import (
	"context"
	"orchestrator/pkg/deployment"
	"testing"
	"time"
)

type fakeBackend struct {
	statuses   []deployment.StatusResponse
	specs      map[string]*deployment.Request
	scaleCalls map[string][]int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{specs: map[string]*deployment.Request{}, scaleCalls: map[string][]int{}}
}

func (f *fakeBackend) List(context.Context) ([]deployment.StatusResponse, error) {
	return f.statuses, nil
}

func (f *fakeBackend) Spec(_ context.Context, id string) (*deployment.Request, error) {
	return f.specs[id], nil
}

func (f *fakeBackend) Scale(_ context.Context, id string, replicas int) error {
	f.scaleCalls[id] = append(f.scaleCalls[id], replicas)
	for i := range f.statuses {
		if f.statuses[i].ID == id {
			f.statuses[i].DesiredReplicas = replicas
		}
	}
	return nil
}

func (f *fakeBackend) add(id string, desired int, auto *deployment.Autoscaling) {
	f.statuses = append(f.statuses, deployment.StatusResponse{
		ID: id, State: deployment.StateReady,
		DesiredReplicas: desired, AvailableReplicas: desired,
	})
	f.specs[id] = &deployment.Request{ID: id, Replicas: desired, Autoscaling: auto}
}

type fixedConcurrency float64

func (f fixedConcurrency) Concurrency(context.Context, string) (float64, error) {
	return float64(f), nil
}

type fixedQueue int

func (f fixedQueue) Queued(context.Context, string) int { return int(f) }

func newTest(backend *fakeBackend, c ConcurrencySource, q QueueSource) *Autoscaler {
	return New(backend, c, q, Config{Tick: time.Second, Window: time.Minute})
}

// tickPast pushes enough sample history that the window guard allows
// scale-downs.
func tickPast(a *Autoscaler, id string) {
	if w := a.windows[id]; w != nil {
		w.firstSeen = time.Now().Add(-2 * time.Minute)
	}
}

func TestEvaluate_ScalesUpWithConcurrency(t *testing.T) {
	backend := newFakeBackend()
	backend.add("web", 1, &deployment.Autoscaling{MinReplicas: 1, MaxReplicas: 5, Target: 10})
	a := newTest(backend, fixedConcurrency(35), fixedQueue(0))

	a.evaluate(t.Context())

	if calls := backend.scaleCalls["web"]; len(calls) != 1 || calls[0] != 4 {
		t.Fatalf("scale calls: want [4] (ceil(35/10)), got %v", calls)
	}
}

func TestEvaluate_ClampsToMax(t *testing.T) {
	backend := newFakeBackend()
	backend.add("web", 1, &deployment.Autoscaling{MinReplicas: 1, MaxReplicas: 3, Target: 1})
	a := newTest(backend, fixedConcurrency(50), fixedQueue(0))

	a.evaluate(t.Context())

	if calls := backend.scaleCalls["web"]; len(calls) != 1 || calls[0] != 3 {
		t.Fatalf("scale calls: want [3] (max clamp), got %v", calls)
	}
}

func TestEvaluate_ScaleDownNeedsFullWindow(t *testing.T) {
	backend := newFakeBackend()
	backend.add("web", 3, &deployment.Autoscaling{MinReplicas: 1, MaxReplicas: 5, Target: 10})
	a := newTest(backend, fixedConcurrency(0), fixedQueue(0))

	// Freshly observed: no scale-down permitted yet.
	a.evaluate(t.Context())
	if calls := backend.scaleCalls["web"]; len(calls) != 0 {
		t.Fatalf("scaled down without a full window of evidence: %v", calls)
	}

	// After a full window of zeros → down to min.
	tickPast(a, "web")
	a.evaluate(t.Context())
	if calls := backend.scaleCalls["web"]; len(calls) != 1 || calls[0] != 1 {
		t.Fatalf("scale calls: want [1] (min), got %v", calls)
	}
}

func TestEvaluate_IdleToZeroWhenMinZero(t *testing.T) {
	backend := newFakeBackend()
	backend.add("web", 1, &deployment.Autoscaling{MinReplicas: 0, MaxReplicas: 5, Target: 10})
	a := newTest(backend, fixedConcurrency(0), fixedQueue(0))

	a.evaluate(t.Context())
	tickPast(a, "web")
	a.evaluate(t.Context())

	if calls := backend.scaleCalls["web"]; len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("scale calls: want [0] (scale-to-zero), got %v", calls)
	}
}

func TestEvaluate_QueuedHoldsUpColdStart(t *testing.T) {
	backend := newFakeBackend()
	backend.add("web", 1, &deployment.Autoscaling{MinReplicas: 0, MaxReplicas: 5, Target: 10})
	// Zero sidecar concurrency (nothing ready yet) but requests queued in the
	// activator: the deployment must never be concluded idle mid-cold-start.
	a := newTest(backend, fixedConcurrency(0), fixedQueue(2))

	a.evaluate(t.Context())
	tickPast(a, "web")
	a.evaluate(t.Context())

	for _, c := range backend.scaleCalls["web"] {
		if c == 0 {
			t.Fatalf("scaled to zero while requests were queued: %v", backend.scaleCalls["web"])
		}
	}
}

func TestEvaluate_NotOptedInUntouched(t *testing.T) {
	backend := newFakeBackend()
	backend.add("fixed", 2, nil)
	a := newTest(backend, fixedConcurrency(100), fixedQueue(5))

	a.evaluate(t.Context())
	tickPast(a, "fixed")
	a.evaluate(t.Context())

	if calls := backend.scaleCalls["fixed"]; len(calls) != 0 {
		t.Fatalf("non-autoscaled deployment scaled: %v", calls)
	}
}

func TestEvaluate_NoWriteWhenStable(t *testing.T) {
	backend := newFakeBackend()
	backend.add("web", 2, &deployment.Autoscaling{MinReplicas: 1, MaxReplicas: 5, Target: 10})
	a := newTest(backend, fixedConcurrency(15), fixedQueue(0)) // ceil(15/10)=2 == current

	a.evaluate(t.Context())

	if calls := backend.scaleCalls["web"]; len(calls) != 0 {
		t.Fatalf("stable deployment written: %v", calls)
	}
}

func TestWindow_AverageSmoothsSpikes(t *testing.T) {
	w := &window{firstSeen: time.Now()}
	now := time.Now()
	for i := range 30 {
		w.push(now.Add(time.Duration(i)*time.Second), 0, time.Minute)
	}
	w.push(now.Add(31*time.Second), 300, time.Minute)

	if avg := w.average(); avg > 15 {
		t.Fatalf("one spike should not dominate a window of zeros: avg=%f", avg)
	}

	// Old samples age out.
	w.push(now.Add(5*time.Minute), 10, time.Minute)
	if avg := w.average(); avg != 10 {
		t.Fatalf("expired samples still counted: avg=%f", avg)
	}
}
