package lifecycle

import (
	"errors"
	"orchestrator/internal/apperrors"
	"testing"
)

// The exit code is the whole rule, and it is one rule: the store applying an
// Exited signal must land where StateForExit says, or an API read and the
// callback for the same exit can disagree — which is exactly what the two jobs
// backends used to do.
func TestApply_AgreesWithStateForExit(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 1, 2, 137, 255} {
		store := NewMemoryStore[struct{}]("job")
		if err := store.Reserve("j"); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if err := store.Apply("j", Started{}); err != nil {
			t.Fatalf("Started: %v", err)
		}
		if err := store.Apply("j", Exited{ExitCode: code}); err != nil {
			t.Fatalf("Exited(%d): %v", code, err)
		}
		entry, _ := store.Get("j")
		if want := StateForExit(code); entry.State != want {
			t.Errorf("exit %d: store says %q, the rule says %q", code, entry.State, want)
		}
		if entry.ExitCode == nil || *entry.ExitCode != code {
			t.Errorf("exit %d: got %v", code, entry.ExitCode)
		}
	}
}

func TestStateForExit(t *testing.T) {
	t.Parallel()
	if got := StateForExit(0); got != StateCompleted {
		t.Errorf("clean exit: got %q", got)
	}
	if got := StateForExit(1); got != StateFailed {
		t.Errorf("failed exit: got %q", got)
	}
}

// A workload can fail without ever starting — an image that will not pull, a
// pre-phase artifact that will not materialize.
func TestApply_FailsWithoutStarting(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore[struct{}]("job")
	if err := store.Reserve("j"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := store.Apply("j", Failed{Reason: "ImagePullBackOff"}); err != nil {
		t.Fatalf("Failed: %v", err)
	}
	entry, _ := store.Get("j")
	if entry.State != StateFailed || entry.Error != "ImagePullBackOff" {
		t.Errorf("got %+v", entry)
	}
	// -1 stands for "never ran", distinct from any code a worker could return.
	if entry.ExitCode == nil || *entry.ExitCode != -1 {
		t.Errorf("exit code: got %v", entry.ExitCode)
	}
}

// The FSM is the guard against a backend reporting a state the workload cannot
// be in — a watcher re-emitting a signal after failover, say.
func TestApply_RefusesToGoBackwards(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore[struct{}]("job")
	if err := store.Reserve("j"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := store.Apply("j", Started{}); err != nil {
		t.Fatalf("Started: %v", err)
	}
	if err := store.Apply("j", Exited{}); err != nil {
		t.Fatalf("Exited: %v", err)
	}
	// Terminal is terminal: nothing follows completion.
	if err := store.Apply("j", Started{}); err == nil {
		t.Error("a completed job cannot start again")
	}
	if err := store.Apply("j", Exited{ExitCode: 1}); err == nil {
		t.Error("a completed job cannot exit again")
	}
	if entry, _ := store.Get("j"); entry.State != StateCompleted {
		t.Errorf("state after refused transitions: got %q", entry.State)
	}
}

// Completed and LogLine carry no state change: they exist for the callback
// stream, and Apply must not treat them as transitions.
func TestApply_IgnoresNonStateSignals(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore[struct{}]("job")
	if err := store.Reserve("j"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	for _, s := range []Signal{Completed{}, LogLine{Stream: "stdout", Lines: []string{"x"}}} {
		if err := store.Apply("j", s); err != nil {
			t.Errorf("%T: %v", s, err)
		}
	}
	if entry, _ := store.Get("j"); entry.State != StateAccepted {
		t.Errorf("state: got %q, want it untouched", entry.State)
	}
}

func TestReserve_RejectsADuplicateID(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore[struct{}]("job")
	if err := store.Reserve("j"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	err := store.Reserve("j")
	if !errors.Is(err, apperrors.ErrConflict) {
		t.Errorf("want a conflict the API can answer with 409, got %v", err)
	}
}

// Release hands back the runtime handle exactly once, so teardown cannot run
// twice on the same container, and a late signal has nowhere to land.
func TestRelease_IsFinal(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore[string]("job")
	if err := store.Reserve("j"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	store.Commit("j", "container-1", func() {})

	handle, ok := store.Release("j")
	if !ok || handle.Runtime != "container-1" {
		t.Fatalf("Release: got %q, %v", handle.Runtime, ok)
	}
	if _, ok := store.Release("j"); ok {
		t.Error("a second Release must find nothing")
	}
	if err := store.Apply("j", Started{}); err == nil {
		t.Error("a released entry accepts no signals")
	}
	if _, ok := store.Get("j"); ok {
		t.Error("a released entry is gone")
	}
}
