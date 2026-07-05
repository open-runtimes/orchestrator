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
	return nil
}

type fakeActivity map[string]time.Time

func (f fakeActivity) LastActivity(id string) (time.Time, bool) {
	t, ok := f[id]
	return t, ok
}

func (f *fakeBackend) add(id string, desired int, scaleToZero bool) {
	f.statuses = append(f.statuses, deployment.StatusResponse{ID: id, State: deployment.StateReady, DesiredReplicas: desired, AvailableReplicas: desired})
	spec := &deployment.Request{ID: id, Replicas: desired}
	if scaleToZero {
		spec.Autoscaling = &deployment.Autoscaling{MinReplicas: 0}
	}
	f.specs[id] = spec
}

func TestEvaluate_ScalesIdleToZero(t *testing.T) {
	backend := newFakeBackend()
	backend.add("idle", 1, true)
	activity := fakeActivity{"idle": time.Now().Add(-2 * time.Minute)}

	a := New(backend, activity, Config{Window: time.Minute, Tick: time.Second})
	a.evaluate(t.Context())

	if calls := backend.scaleCalls["idle"]; len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("scale calls: want [0], got %v", calls)
	}
}

func TestEvaluate_ActiveStaysUp(t *testing.T) {
	backend := newFakeBackend()
	backend.add("busy", 1, true)
	activity := fakeActivity{"busy": time.Now().Add(-5 * time.Second)}

	a := New(backend, activity, Config{Window: time.Minute, Tick: time.Second})
	a.evaluate(t.Context())

	if calls := backend.scaleCalls["busy"]; len(calls) != 0 {
		t.Fatalf("active deployment scaled: %v", calls)
	}
}

func TestEvaluate_NotOptedInIsUntouched(t *testing.T) {
	backend := newFakeBackend()
	backend.add("fixed", 1, false)
	activity := fakeActivity{"fixed": time.Now().Add(-time.Hour)}

	a := New(backend, activity, Config{Window: time.Minute, Tick: time.Second})
	a.evaluate(t.Context())

	if calls := backend.scaleCalls["fixed"]; len(calls) != 0 {
		t.Fatalf("non-autoscaled deployment scaled: %v", calls)
	}
}

func TestEvaluate_NeverActiveGetsFullWindowFromFirstSight(t *testing.T) {
	backend := newFakeBackend()
	backend.add("fresh", 1, true)
	activity := fakeActivity{} // no traffic ever observed

	a := New(backend, activity, Config{Window: time.Minute, Tick: time.Second})
	a.evaluate(t.Context()) // first sight: baseline recorded, no scale
	if calls := backend.scaleCalls["fresh"]; len(calls) != 0 {
		t.Fatalf("scaled on first sight: %v", calls)
	}

	// Simulate the window elapsing since first sight.
	a.firstSeen["fresh"] = time.Now().Add(-2 * time.Minute)
	a.evaluate(t.Context())
	if calls := backend.scaleCalls["fresh"]; len(calls) != 1 || calls[0] != 0 {
		t.Fatalf("want scale to zero after window from first sight, got %v", calls)
	}
}

func TestEvaluate_AlreadyZeroSkipped(t *testing.T) {
	backend := newFakeBackend()
	backend.statuses = append(backend.statuses, deployment.StatusResponse{ID: "cold", State: deployment.StateIdle, DesiredReplicas: 0})
	backend.specs["cold"] = &deployment.Request{ID: "cold", Autoscaling: &deployment.Autoscaling{MinReplicas: 0}}
	activity := fakeActivity{"cold": time.Now().Add(-time.Hour)}

	a := New(backend, activity, Config{Window: time.Minute, Tick: time.Second})
	a.evaluate(t.Context())

	if calls := backend.scaleCalls["cold"]; len(calls) != 0 {
		t.Fatalf("already-zero deployment written: %v", calls)
	}
}
