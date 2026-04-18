package docker

import (
	"context"
	"orchestrator/internal/job"
	"sync"
	"testing"
	"time"
)

// fakeWatcher replays a fixed sequence of events with no Docker daemon.
type fakeWatcher struct {
	events []job.Signal
}

func (f *fakeWatcher) Watch(_ context.Context, _, _ string, emit func(job.Signal)) {
	for _, e := range f.events {
		emit(e)
	}
}

// blockingWatcher blocks until ctx is cancelled.
type blockingWatcher struct{}

func (b *blockingWatcher) Watch(ctx context.Context, _, _ string, _ func(job.Signal)) {
	<-ctx.Done()
}

// captureListener records all emitted job events.
type captureListener struct {
	mu     sync.Mutex
	events []*job.Event
}

func (c *captureListener) record(e *job.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureListener) all() []*job.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]*job.Event, len(c.events))
	copy(result, c.events)
	return result
}

func (c *captureListener) types() []string {
	all := c.all()
	types := make([]string, len(all))
	for i, e := range all {
		types[i] = e.Payload.Type
	}
	return types
}

// --- tests ---

func TestLifecycle_HappyPath(t *testing.T) {
	t.Parallel()
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.Register(capture.record)
	dest := &job.CallbackDest{URL: "http://example.com/cb"}

	w := &fakeWatcher{events: []job.Signal{
		job.Started{},
		job.Exited{ExitCode: 0, Duration: 2 * time.Second},
	}}
	w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
		_ = ctrl.Apply("job-1", s)
		job.EmitCallback(emitter, "job-1", "alpine:latest", dest, s)
	})

	entry, _ := ctrl.Get("job-1")
	if entry.State != job.StateCompleted {
		t.Errorf("want StateCompleted, got %s", entry.State)
	}
	if entry.ExitCode == nil || *entry.ExitCode != 0 {
		t.Errorf("want exit code 0, got %v", entry.ExitCode)
	}
	got := capture.types()
	want := []string{job.EventTypeStart, job.EventTypeExit}
	if len(got) != len(want) {
		t.Fatalf("want events %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d]: want %s, got %s", i, want[i], got[i])
		}
	}
}

func TestLifecycle_WorkerFailure(t *testing.T) {
	t.Parallel()
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.Register(capture.record)
	dest := &job.CallbackDest{URL: "http://example.com/cb"}

	w := &fakeWatcher{events: []job.Signal{
		job.Started{},
		job.Exited{ExitCode: 1, Duration: time.Second},
	}}
	w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
		_ = ctrl.Apply("job-1", s)
		job.EmitCallback(emitter, "job-1", "alpine:latest", dest, s)
	})

	entry, _ := ctrl.Get("job-1")
	if entry.State != job.StateFailed {
		t.Errorf("want StateFailed, got %s", entry.State)
	}
	if entry.ExitCode == nil || *entry.ExitCode != 1 {
		t.Errorf("want exit code 1, got %v", entry.ExitCode)
	}
	got := capture.types()
	if len(got) != 2 || got[0] != job.EventTypeStart || got[1] != job.EventTypeExit {
		t.Errorf("want [start, exit], got %v", got)
	}
}

func TestLifecycle_SidecarCrashBeforeWorker(t *testing.T) {
	t.Parallel()
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.Register(capture.record)
	dest := &job.CallbackDest{URL: "http://example.com/cb"}

	w := &fakeWatcher{events: []job.Signal{
		job.Failed{Reason: "sidecar exited before inputs completed"},
	}}
	w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
		_ = ctrl.Apply("job-1", s)
		job.EmitCallback(emitter, "job-1", "alpine:latest", dest, s)
	})

	entry, _ := ctrl.Get("job-1")
	if entry.State != job.StateFailed {
		t.Errorf("want StateFailed, got %s", entry.State)
	}
	if entry.ExitCode == nil || *entry.ExitCode != -1 {
		t.Errorf("want exit code -1, got %v", entry.ExitCode)
	}
	got := capture.types()
	if len(got) != 1 || got[0] != job.EventTypeExit {
		t.Errorf("want [exit], got %v", got)
	}
}

func TestLifecycle_LogDelivered(t *testing.T) {
	t.Parallel()
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.Register(capture.record)
	dest := &job.CallbackDest{URL: "http://example.com/cb"}

	// No event filter = all events allowed
	w := &fakeWatcher{events: []job.Signal{
		job.Started{},
		job.LogLine{Stream: "stdout", Lines: []string{"hello", "world"}},
		job.Exited{ExitCode: 0},
	}}
	w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
		_ = ctrl.Apply("job-1", s)
		job.EmitCallback(emitter, "job-1", "alpine:latest", dest, s)
	})

	got := capture.types()
	want := []string{job.EventTypeStart, job.EventTypeLog, job.EventTypeExit}
	if len(got) != len(want) {
		t.Fatalf("want events %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d]: want %s, got %s", i, want[i], got[i])
		}
	}
}

func TestLifecycle_LogSkippedWhenNoCallback(t *testing.T) {
	t.Parallel()
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.Register(capture.record)

	w := &fakeWatcher{events: []job.Signal{
		job.Started{},
		job.LogLine{Stream: "stdout", Lines: []string{"hello"}},
		job.Exited{ExitCode: 0},
	}}
	w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
		_ = ctrl.Apply("job-1", s)
		job.EmitCallback(emitter, "job-1", "alpine:latest", nil, s)
	})

	for _, e := range capture.all() {
		if e.Payload.Type == job.EventTypeLog {
			t.Error("log event should not be emitted when no callback is configured")
		}
	}
}

func TestLifecycle_LogSkippedWhenFilteredOut(t *testing.T) {
	t.Parallel()
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.Register(capture.record)
	dest := &job.CallbackDest{URL: "http://example.com/cb", Events: []string{job.EventTypeStart, job.EventTypeExit}}

	w := &fakeWatcher{events: []job.Signal{
		job.Started{},
		job.LogLine{Stream: "stderr", Lines: []string{"warning"}},
		job.Exited{ExitCode: 0},
	}}
	w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
		_ = ctrl.Apply("job-1", s)
		job.EmitCallback(emitter, "job-1", "alpine:latest", dest, s)
	})

	for _, e := range capture.all() {
		if e.Payload.Type == job.EventTypeLog {
			t.Error("log event should not be emitted when not in callback filter")
		}
	}
}

func TestLifecycle_ResumeNoStarted(t *testing.T) {
	t.Parallel()
	// Simulate a resumed job: worker was already running, so Watch emits
	// Exited without a leading Started.
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	_ = ctrl.Apply("job-1", job.Started{})
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.Register(capture.record)
	dest := &job.CallbackDest{URL: "http://example.com/cb"}

	w := &fakeWatcher{events: []job.Signal{
		job.Exited{ExitCode: 0, Duration: 5 * time.Second},
	}}
	w.Watch(t.Context(), "sc-1", "wk-1", func(s job.Signal) {
		_ = ctrl.Apply("job-1", s)
		job.EmitCallback(emitter, "job-1", "alpine:latest", dest, s)
	})

	entry, _ := ctrl.Get("job-1")
	if entry.State != job.StateCompleted {
		t.Errorf("want StateCompleted, got %s", entry.State)
	}
	for _, e := range capture.all() {
		if e.Payload.Type == job.EventTypeStart {
			t.Error("start event should not be emitted for resumed job")
		}
	}
	types := capture.types()
	if len(types) != 1 || types[0] != job.EventTypeExit {
		t.Errorf("want [exit] event, got %v", types)
	}
}

func TestLifecycle_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctrl := job.NewMemoryStore[dockerHandle]()
	_ = ctrl.Reserve("job-1")
	ctrl.Commit("job-1", dockerHandle{}, nil)
	emitter := job.NewEventEmitter()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&blockingWatcher{}).Watch(ctx, "sc-1", "wk-1", func(s job.Signal) {
			_ = ctrl.Apply("job-1", s)
			job.EmitCallback(emitter, "job-1", "alpine:latest", nil, s)
		})
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}
