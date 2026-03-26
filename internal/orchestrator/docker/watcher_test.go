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
	events []JobEvent
}

func (f *fakeWatcher) Watch(_ context.Context, _, _ string) <-chan JobEvent {
	ch := make(chan JobEvent, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return ch
}

// captureListener records all emitted job events.
type captureListener struct {
	mu     sync.Mutex
	events []*job.Event
}

func (c *captureListener) OnJobEvent(e *job.Event) {
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

// helpers

func newTestOrchestrator(w JobWatcher) (*Orchestrator, *dockerRegistry, *captureListener) {
	reg := newDockerRegistry()
	emitter := job.NewEventEmitter()
	capture := &captureListener{}
	emitter.OnEvent(capture)
	return &Orchestrator{registry: reg, emitter: emitter, watcher: w}, reg, capture
}

func testCfgNoCallback(jobID string) *watchConfig {
	return &watchConfig{jobID: jobID, image: "alpine:latest", sidecarID: "sc-1", workerID: "wk-1"}
}

func testCfgWithCallback(jobID string, events ...string) *watchConfig {
	return &watchConfig{
		jobID:     jobID,
		image:     "alpine:latest",
		sidecarID: "sc-1",
		workerID:  "wk-1",
		dest: &callbackDest{
			jobID:  jobID,
			url:    "http://example.com/cb",
			events: events,
		},
	}
}

// --- runWatchLoop tests ---

func TestRunWatchLoop_HappyPath(t *testing.T) {
	t.Parallel()
	script := []JobEvent{
		SidecarReady{},
		WorkerExited{ExitCode: 0, Duration: 2 * time.Second},
		SidecarExited{WorkerEverStarted: true},
	}
	o, reg, capture := newTestOrchestrator(&fakeWatcher{events: script})
	_ = reg.Reserve("job-1")
	cfg := testCfgWithCallback("job-1")

	o.runWatchLoop(context.Background(), cfg)

	entry, _ := reg.Get("job-1")
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

func TestRunWatchLoop_WorkerFailure(t *testing.T) {
	t.Parallel()
	script := []JobEvent{
		SidecarReady{},
		WorkerExited{ExitCode: 1, Duration: time.Second},
		SidecarExited{WorkerEverStarted: true},
	}
	o, reg, capture := newTestOrchestrator(&fakeWatcher{events: script})
	_ = reg.Reserve("job-1")
	cfg := testCfgWithCallback("job-1")

	o.runWatchLoop(context.Background(), cfg)

	entry, _ := reg.Get("job-1")
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

func TestRunWatchLoop_SidecarCrashBeforeWorker(t *testing.T) {
	t.Parallel()
	script := []JobEvent{
		SidecarExited{WorkerEverStarted: false},
	}
	o, reg, capture := newTestOrchestrator(&fakeWatcher{events: script})
	_ = reg.Reserve("job-1")
	cfg := testCfgWithCallback("job-1")

	o.runWatchLoop(context.Background(), cfg)

	entry, _ := reg.Get("job-1")
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

func TestRunWatchLoop_LogDelivered(t *testing.T) {
	t.Parallel()
	script := []JobEvent{
		SidecarReady{},
		LogLine{Stream: "stdout", Lines: []string{"hello", "world"}},
		WorkerExited{ExitCode: 0},
		SidecarExited{WorkerEverStarted: true},
	}
	// No event filter = all events allowed
	o, reg, capture := newTestOrchestrator(&fakeWatcher{events: script})
	_ = reg.Reserve("job-1")
	cfg := testCfgWithCallback("job-1")

	o.runWatchLoop(context.Background(), cfg)

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

func TestRunWatchLoop_LogSkippedWhenNoCallback(t *testing.T) {
	t.Parallel()
	script := []JobEvent{
		SidecarReady{},
		LogLine{Stream: "stdout", Lines: []string{"hello"}},
		WorkerExited{ExitCode: 0},
		SidecarExited{WorkerEverStarted: true},
	}
	o, reg, capture := newTestOrchestrator(&fakeWatcher{events: script})
	_ = reg.Reserve("job-1")

	o.runWatchLoop(context.Background(), testCfgNoCallback("job-1"))

	for _, e := range capture.all() {
		if e.Payload.Type == job.EventTypeLog {
			t.Error("log event should not be emitted when no callback is configured")
		}
	}
}

func TestRunWatchLoop_LogSkippedWhenFilteredOut(t *testing.T) {
	t.Parallel()
	script := []JobEvent{
		SidecarReady{},
		LogLine{Stream: "stderr", Lines: []string{"warning"}},
		WorkerExited{ExitCode: 0},
		SidecarExited{WorkerEverStarted: true},
	}
	// Callback configured but log events not in filter
	o, reg, capture := newTestOrchestrator(&fakeWatcher{events: script})
	_ = reg.Reserve("job-1")
	cfg := testCfgWithCallback("job-1", job.EventTypeStart, job.EventTypeExit)

	o.runWatchLoop(context.Background(), cfg)

	for _, e := range capture.all() {
		if e.Payload.Type == job.EventTypeLog {
			t.Error("log event should not be emitted when not in callback filter")
		}
	}
}

func TestRunWatchLoop_ResumeNoSidecarReady(t *testing.T) {
	t.Parallel()
	// Simulate a resumed job: worker was already running, so Watch emits
	// WorkerExited and SidecarExited without a leading SidecarReady.
	script := []JobEvent{
		WorkerExited{ExitCode: 0, Duration: 5 * time.Second},
		SidecarExited{WorkerEverStarted: true},
	}
	o, reg, capture := newTestOrchestrator(&fakeWatcher{events: script})
	// Outer reconcile already set Running before spawning the goroutine.
	_ = reg.Restore("job-1", job.ToRunning(), dockerHandle{}, nil)
	cfg := testCfgWithCallback("job-1")

	o.runWatchLoop(context.Background(), cfg)

	entry, _ := reg.Get("job-1")
	if entry.State != job.StateCompleted {
		t.Errorf("want StateCompleted, got %s", entry.State)
	}

	// No start event should be emitted — worker was already running.
	for _, e := range capture.all() {
		if e.Payload.Type == job.EventTypeStart {
			t.Error("start event should not be emitted for resumed job")
		}
	}
	// Exit event should still be emitted.
	types := capture.types()
	if len(types) != 1 || types[0] != job.EventTypeExit {
		t.Errorf("want [exit] event, got %v", types)
	}
}

func TestRunWatchLoop_SidecarExitedAfterWorker(t *testing.T) {
	t.Parallel()
	// SidecarExited{WorkerEverStarted: true} should not change state
	// (worker exit already handled it).
	script := []JobEvent{
		SidecarReady{},
		WorkerExited{ExitCode: 0},
		SidecarExited{WorkerEverStarted: true},
	}
	o, reg, _ := newTestOrchestrator(&fakeWatcher{events: script})
	_ = reg.Reserve("job-1")

	o.runWatchLoop(context.Background(), testCfgNoCallback("job-1"))

	entry, _ := reg.Get("job-1")
	if entry.State != job.StateCompleted {
		t.Errorf("want StateCompleted, got %s", entry.State)
	}
}

func TestRunWatchLoop_ContextCancelled(t *testing.T) {
	t.Parallel()
	blocking := &blockingWatcher{}
	o, reg, _ := newTestOrchestrator(blocking)
	_ = reg.Reserve("job-1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.runWatchLoop(ctx, testCfgNoCallback("job-1"))
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runWatchLoop did not return after context cancellation")
	}
}

// blockingWatcher returns a channel that is only closed when ctx is cancelled.
type blockingWatcher struct{}

func (b *blockingWatcher) Watch(ctx context.Context, _, _ string) <-chan JobEvent {
	ch := make(chan JobEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}
