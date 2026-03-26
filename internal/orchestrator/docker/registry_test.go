package docker

import (
	"context"
	"orchestrator/internal/job"
	"sync"
	"testing"
)

func TestRegistry_Reserve(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()

	if err := r.Reserve("job-1"); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}

	// Entry should exist in Accepted state.
	entry, ok := r.Get("job-1")
	if !ok {
		t.Fatal("Get: expected entry after Reserve")
	}
	if entry.State != job.StateAccepted {
		t.Errorf("State: want %s, got %s", job.StateAccepted, entry.State)
	}
}

func TestRegistry_Reserve_Duplicate(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()

	_ = r.Reserve("job-1")
	if err := r.Reserve("job-1"); err == nil {
		t.Error("second Reserve: expected error, got nil")
	}
}

func TestRegistry_Commit(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()

	_ = r.Reserve("job-1")
	cancelled := false
	r.Commit("job-1", dockerHandle{jobContainerID: "c-1", sidecarContainerID: "sc-1", volumeName: "v-1"}, func() {
		cancelled = true
	})

	h, ok := r.Release("job-1")
	if !ok {
		t.Fatal("Release: expected entry")
	}
	if h.Runtime.jobContainerID != "c-1" {
		t.Errorf("jobContainerID: want c-1, got %s", h.Runtime.jobContainerID)
	}
	if h.Runtime.sidecarContainerID != "sc-1" {
		t.Errorf("sidecarContainerID: want sc-1, got %s", h.Runtime.sidecarContainerID)
	}
	if h.CancelWatch == nil {
		t.Fatal("CancelWatch: expected non-nil")
	}
	h.CancelWatch()
	if !cancelled {
		t.Error("CancelWatch did not call the registered function")
	}
}

func TestRegistry_Apply_ValidTransitions(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")

	if err := r.Apply("job-1", job.ToRunning()); err != nil {
		t.Fatalf("Apply ToRunning: %v", err)
	}
	e, _ := r.Get("job-1")
	if e.State != job.StateRunning {
		t.Errorf("State: want %s, got %s", job.StateRunning, e.State)
	}

	if err := r.Apply("job-1", job.ToCompleted(0)); err != nil {
		t.Fatalf("Apply ToCompleted: %v", err)
	}
	e, _ = r.Get("job-1")
	if e.State != job.StateCompleted {
		t.Errorf("State: want %s, got %s", job.StateCompleted, e.State)
	}
	if e.ExitCode == nil || *e.ExitCode != 0 {
		t.Errorf("ExitCode: want 0, got %v", e.ExitCode)
	}
}

func TestRegistry_Apply_InvalidTransition(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")

	// Accepted → Completed is not valid; must go through Running.
	if err := r.Apply("job-1", job.ToCompleted(0)); err == nil {
		t.Error("Apply: expected error for Accepted→Completed, got nil")
	}
}

func TestRegistry_Apply_WithExitCodeAndError(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")
	_ = r.Apply("job-1", job.ToFailed(1, "oom killed"))

	e, _ := r.Get("job-1")
	if e.State != job.StateFailed {
		t.Errorf("State: want %s, got %s", job.StateFailed, e.State)
	}
	if e.ExitCode == nil || *e.ExitCode != 1 {
		t.Errorf("ExitCode: want 1, got %v", e.ExitCode)
	}
	if e.Error != "oom killed" {
		t.Errorf("Error: want 'oom killed', got %q", e.Error)
	}
}

func TestRegistry_Apply_NotFound(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	if err := r.Apply("nonexistent", job.ToRunning()); err == nil {
		t.Error("Apply on nonexistent job: expected error, got nil")
	}
}

func TestRegistry_Release(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")
	r.Commit("job-1", dockerHandle{volumeName: "v-1"}, nil)

	h, ok := r.Release("job-1")
	if !ok {
		t.Fatal("Release: expected ok=true")
	}
	if h.Runtime.volumeName != "v-1" {
		t.Errorf("volumeName: want v-1, got %s", h.Runtime.volumeName)
	}

	// Entry should no longer exist.
	if _, exists := r.Get("job-1"); exists {
		t.Error("Get after Release: expected not found")
	}

	// Second Release should return false.
	if _, ok := r.Release("job-1"); ok {
		t.Error("second Release: expected ok=false")
	}
}

func TestRegistry_Restore(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()

	h := dockerHandle{jobContainerID: "c-1", sidecarContainerID: "sc-1", volumeName: "v-1"}
	if err := r.Restore("job-1", job.ToCompleted(0), h, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	e, ok := r.Get("job-1")
	if !ok {
		t.Fatal("Get after Restore: expected entry")
	}
	if e.State != job.StateCompleted {
		t.Errorf("State: want %s, got %s", job.StateCompleted, e.State)
	}
	if e.ExitCode == nil || *e.ExitCode != 0 {
		t.Errorf("ExitCode: want 0, got %v", e.ExitCode)
	}
}

func TestRegistry_Restore_Duplicate(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Restore("job-1", job.ToCompleted(0), dockerHandle{}, nil)
	if err := r.Restore("job-1", job.ToCompleted(0), dockerHandle{}, nil); err == nil {
		t.Error("second Restore: expected error, got nil")
	}
}

func TestRegistry_List(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")
	_ = r.Reserve("job-2")

	entries := r.List()
	if len(entries) != 2 {
		t.Errorf("List: want 2 entries, got %d", len(entries))
	}
}

func TestRegistry_Each(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")
	_ = r.Reserve("job-2")

	seen := make(map[string]bool)
	r.Each(func(jobID string, _ job.Entry, _ job.Handle[dockerHandle]) {
		seen[jobID] = true
	})
	if !seen["job-1"] || !seen["job-2"] {
		t.Errorf("Each: did not visit all jobs, saw %v", seen)
	}
}

func TestRegistry_Each_ReleaseSafe(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")
	_ = r.Reserve("job-2")

	// Releasing inside Each should not deadlock.
	r.Each(func(jobID string, _ job.Entry, _ job.Handle[dockerHandle]) {
		r.Release(jobID)
	})

	if len(r.List()) != 0 {
		t.Error("Expected all jobs released")
	}
}

func TestRegistry_ConcurrentReserve(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()

	const n = 100
	results := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			results <- r.Reserve("contested")
		}()
	}
	wg.Wait()
	close(results)

	var successes int
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("ConcurrentReserve: want exactly 1 success, got %d", successes)
	}
}

func TestRegistry_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()

	for i := 0; i < 10; i++ {
		_ = r.Reserve(string(rune('a' + i)))
	}

	var wg sync.WaitGroup
	const ops = 100

	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.List()
			_, _ = r.Get("a")
		}()
	}

	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('A' + i%26))
			if err := r.Reserve(id); err == nil {
				r.Commit(id, dockerHandle{}, nil)
			}
		}(i)
	}

	wg.Wait()
}

func TestRegistry_WatchCancelOnRelease(t *testing.T) {
	t.Parallel()
	r := newDockerRegistry()
	_ = r.Reserve("job-1")

	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx

	r.Commit("job-1", dockerHandle{}, cancel)
	h, _ := r.Release("job-1")

	// The cancel function should be returned so the caller can stop the watcher.
	if h.CancelWatch == nil {
		t.Fatal("CancelWatch: expected non-nil after Release")
	}
}
