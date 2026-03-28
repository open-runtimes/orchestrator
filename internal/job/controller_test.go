package job

import (
	"sync"
	"testing"
)

// newTestController is a convenience helper for tests that don't care about
// the runtime handle type.
func newTestController() *MemoryStore[struct{}] {
	return NewMemoryStore[struct{}]()
}

func TestMemoryStore_Reserve_CreatesAccepted(t *testing.T) {
	c := newTestController()

	if err := c.Reserve("job-1"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	entry, ok := c.Get("job-1")
	if !ok {
		t.Fatal("Get: expected entry after Reserve")
	}
	if entry.State != StateAccepted {
		t.Errorf("State: want %s, got %s", StateAccepted, entry.State)
	}
	if entry.ID != "job-1" {
		t.Errorf("ID: want job-1, got %s", entry.ID)
	}
	if entry.CreatedAt.IsZero() {
		t.Error("CreatedAt: expected non-zero")
	}
	if entry.UpdatedAt.IsZero() {
		t.Error("UpdatedAt: expected non-zero")
	}
}

func TestMemoryStore_Reserve_Duplicate(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")

	if err := c.Reserve("job-1"); err == nil {
		t.Error("second Reserve: expected error, got nil")
	}
}

func TestMemoryStore_Commit_StoresHandle(t *testing.T) {
	type handle struct{ id string }
	c := NewMemoryStore[handle]()

	_ = c.Reserve("job-1")
	cancelled := false
	_ = c.Commit("job-1", handle{id: "c-1"}, func() { cancelled = true })

	h, ok := c.Release("job-1")
	if !ok {
		t.Fatal("Release: expected entry")
	}
	if h.Runtime.id != "c-1" {
		t.Errorf("Runtime.id: want c-1, got %s", h.Runtime.id)
	}
	if h.CancelWatch == nil {
		t.Fatal("CancelWatch: expected non-nil")
	}
	h.CancelWatch()
	if !cancelled {
		t.Error("CancelWatch did not invoke the registered function")
	}
}

func TestMemoryStore_Notify_ValidTransitions(t *testing.T) {
	transitions := []struct {
		from string
		via  []Transition
		want string
	}{
		{"accepted→running", []Transition{ToRunning()}, StateRunning},
		{"accepted→failed", []Transition{ToFailed(1, "")}, StateFailed},
		{"accepted→cancelled", []Transition{ToCancelled()}, StateCancelled},
		{"running→completed", []Transition{ToRunning(), ToCompleted(0)}, StateCompleted},
		{"running→failed", []Transition{ToRunning(), ToFailed(2, "oom")}, StateFailed},
		{"running→cancelled", []Transition{ToRunning(), ToCancelled()}, StateCancelled},
	}

	for _, tt := range transitions {
		t.Run(tt.from, func(t *testing.T) {
			c := newTestController()
			_ = c.Reserve("job-1")
			n := c.Commit("job-1", struct{}{}, nil)

			for _, tr := range tt.via {
				if err := n.Notify(tr); err != nil {
					t.Fatalf("Notify(%s): %v", tr.State(), err)
				}
			}

			entry, _ := c.Get("job-1")
			if entry.State != tt.want {
				t.Errorf("State: want %s, got %s", tt.want, entry.State)
			}
		})
	}
}

func TestMemoryStore_Notify_InvalidTransitions(t *testing.T) {
	invalid := []struct {
		name string
		via  []Transition
		bad  Transition
	}{
		{"accepted→completed", nil, ToCompleted(0)},
		{"running→accepted", []Transition{ToRunning()}, ToAccepted()},
		{"completed→running", []Transition{ToRunning(), ToCompleted(0)}, ToRunning()},
		{"failed→running", []Transition{ToFailed(1, "")}, ToRunning()},
	}

	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestController()
			_ = c.Reserve("job-1")
			n := c.Commit("job-1", struct{}{}, nil)

			for _, tr := range tt.via {
				_ = n.Notify(tr)
			}

			if err := n.Notify(tt.bad); err == nil {
				t.Errorf("Notify(%s): expected error, got nil", tt.bad.State())
			}
		})
	}
}

func TestMemoryStore_Notify_SetsExitCodeAndError(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	n := c.Commit("job-1", struct{}{}, nil)
	_ = n.Notify(ToRunning())
	_ = n.Notify(ToFailed(1, "oom killed"))

	entry, _ := c.Get("job-1")
	if entry.State != StateFailed {
		t.Errorf("State: want %s, got %s", StateFailed, entry.State)
	}
	if entry.ExitCode == nil || *entry.ExitCode != 1 {
		t.Errorf("ExitCode: want 1, got %v", entry.ExitCode)
	}
	if entry.Error != "oom killed" {
		t.Errorf("Error: want 'oom killed', got %q", entry.Error)
	}
}

func TestMemoryStore_Notify_AfterRelease(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	n := c.Commit("job-1", struct{}{}, nil)
	c.Release("job-1")

	if err := n.Notify(ToRunning()); err == nil {
		t.Error("Notify after Release: expected error, got nil")
	}
}

func TestMemoryStore_Restore_CreatesEntry(t *testing.T) {
	c := newTestController()

	n, err := c.Restore("job-1", ToCompleted(0), struct{}{}, nil)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n == nil {
		t.Fatal("Restore: expected non-nil Notifier")
	}

	entry, ok := c.Get("job-1")
	if !ok {
		t.Fatal("Get after Restore: expected entry")
	}
	if entry.State != StateCompleted {
		t.Errorf("State: want %s, got %s", StateCompleted, entry.State)
	}
	if entry.ExitCode == nil || *entry.ExitCode != 0 {
		t.Errorf("ExitCode: want 0, got %v", entry.ExitCode)
	}
}

func TestMemoryStore_Restore_Duplicate(t *testing.T) {
	c := newTestController()
	_, _ = c.Restore("job-1", ToCompleted(0), struct{}{}, nil)

	if _, err := c.Restore("job-1", ToCompleted(0), struct{}{}, nil); err == nil {
		t.Error("second Restore: expected error, got nil")
	}
}

func TestMemoryStore_Restore_NotifierWorks(t *testing.T) {
	c := newTestController()
	n, _ := c.Restore("job-1", ToAccepted(), struct{}{}, nil)

	if err := n.Notify(ToRunning()); err != nil {
		t.Fatalf("Notify on restored job: %v", err)
	}

	entry, _ := c.Get("job-1")
	if entry.State != StateRunning {
		t.Errorf("State: want %s, got %s", StateRunning, entry.State)
	}
}

func TestMemoryStore_Release_RemovesEntry(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")

	h, ok := c.Release("job-1")
	if !ok {
		t.Fatal("Release: expected ok=true")
	}
	_ = h

	if _, exists := c.Get("job-1"); exists {
		t.Error("Get after Release: expected not found")
	}

	if _, ok := c.Release("job-1"); ok {
		t.Error("second Release: expected ok=false")
	}
}

func TestMemoryStore_Get_NotFound(t *testing.T) {
	c := newTestController()
	_, ok := c.Get("nonexistent")
	if ok {
		t.Error("Get: expected ok=false for nonexistent job")
	}
}

func TestMemoryStore_Get_ReturnsCopy(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")

	entry, _ := c.Get("job-1")
	entry.State = "mutated"

	original, _ := c.Get("job-1")
	if original.State != StateAccepted {
		t.Error("Get should return a copy, not a reference")
	}
}

func TestMemoryStore_List_Empty(t *testing.T) {
	c := newTestController()
	if entries := c.List(); len(entries) != 0 {
		t.Errorf("List: want 0 entries, got %d", len(entries))
	}
}

func TestMemoryStore_List_Populated(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	_ = c.Reserve("job-2")

	entries := c.List()
	if len(entries) != 2 {
		t.Fatalf("List: want 2 entries, got %d", len(entries))
	}

	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID] = true
	}
	if !ids["job-1"] || !ids["job-2"] {
		t.Errorf("List: expected job-1 and job-2, got %v", ids)
	}
}

func TestMemoryStore_Each_VisitsAll(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	_ = c.Reserve("job-2")

	seen := map[string]bool{}
	c.Each(func(jobID string, _ Entry, _ Handle[struct{}]) {
		seen[jobID] = true
	})
	if !seen["job-1"] || !seen["job-2"] {
		t.Errorf("Each: did not visit all jobs, saw %v", seen)
	}
}

func TestMemoryStore_Each_ReleaseSafe(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	_ = c.Reserve("job-2")

	// Releasing inside Each must not deadlock.
	c.Each(func(jobID string, _ Entry, _ Handle[struct{}]) {
		c.Release(jobID)
	})

	if len(c.List()) != 0 {
		t.Error("Expected all jobs released after Each+Release")
	}
}

func TestMemoryStore_ConcurrentReserve(t *testing.T) {
	t.Parallel()
	c := newTestController()

	const n = 100
	results := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			results <- c.Reserve("contested")
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

func TestMemoryStore_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	c := newTestController()

	for i := 0; i < 10; i++ {
		_ = c.Reserve(string(rune('a' + i)))
	}

	var wg sync.WaitGroup
	const ops = 100

	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.List()
			_, _ = c.Get("a")
		}()
	}

	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('A' + i%26))
			if err := c.Reserve(id); err == nil {
				_ = c.Commit(id, struct{}{}, nil)
			}
		}(i)
	}

	wg.Wait()
}

func TestTransitionForExit(t *testing.T) {
	if tr := TransitionForExit(0); tr.State() != StateCompleted {
		t.Errorf("exit 0: want %s, got %s", StateCompleted, tr.State())
	}
	if tr := TransitionForExit(1); tr.State() != StateFailed {
		t.Errorf("exit 1: want %s, got %s", StateFailed, tr.State())
	}
	if tr := TransitionForExit(137); tr.State() != StateFailed {
		t.Errorf("exit 137: want %s, got %s", StateFailed, tr.State())
	}
}

func TestEntry_Status(t *testing.T) {
	exitCode := 1
	entry := &Entry{
		ID:       "job-1",
		State:    StateFailed,
		ExitCode: &exitCode,
		Error:    "signal: killed",
	}

	status := entry.Status()
	if status.ID != "job-1" {
		t.Errorf("ID: want job-1, got %s", status.ID)
	}
	if status.State != StateFailed {
		t.Errorf("State: want %s, got %s", StateFailed, status.State)
	}
	if status.ExitCode == nil || *status.ExitCode != 1 {
		t.Errorf("ExitCode: want 1, got %v", status.ExitCode)
	}
	if status.Error != "signal: killed" {
		t.Errorf("Error: want 'signal: killed', got %q", status.Error)
	}

	// Verify ExitCode is a copy.
	*status.ExitCode = 99
	if *entry.ExitCode != 1 {
		t.Error("Status() should return a copy of ExitCode")
	}
}

func TestEntry_Status_NoExitCode(t *testing.T) {
	entry := &Entry{ID: "job-1", State: StateRunning}
	status := entry.Status()
	if status.ExitCode != nil {
		t.Errorf("ExitCode: want nil, got %v", status.ExitCode)
	}
}
