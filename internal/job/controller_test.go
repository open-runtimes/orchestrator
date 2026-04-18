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
	c.Commit("job-1", handle{id: "c-1"}, func() { cancelled = true })

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

func TestMemoryStore_Apply_ValidSignals(t *testing.T) {
	tests := []struct {
		name    string
		signals []Signal
		want    string
	}{
		{"accepted→running", []Signal{Started{}}, StateRunning},
		{"accepted→failed", []Signal{Failed{Reason: "crash"}}, StateFailed},
		{"running→completed", []Signal{Started{}, Exited{ExitCode: 0}}, StateCompleted},
		{"running→failed", []Signal{Started{}, Exited{ExitCode: 2}}, StateFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestController()
			_ = c.Reserve("job-1")
			c.Commit("job-1", struct{}{}, nil)

			for _, s := range tt.signals {
				if err := c.Apply("job-1", s); err != nil {
					t.Fatalf("Apply(%T): %v", s, err)
				}
			}

			entry, _ := c.Get("job-1")
			if entry.State != tt.want {
				t.Errorf("State: want %s, got %s", tt.want, entry.State)
			}
		})
	}
}

func TestMemoryStore_Apply_InvalidSignals(t *testing.T) {
	tests := []struct {
		name  string
		setup []Signal
		bad   Signal
	}{
		{"exited from accepted", nil, Exited{ExitCode: 0}},
		{"started when already running", []Signal{Started{}}, Started{}},
		{"started when completed", []Signal{Started{}, Exited{ExitCode: 0}}, Started{}},
		{"started when failed", []Signal{Failed{}}, Started{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestController()
			_ = c.Reserve("job-1")
			c.Commit("job-1", struct{}{}, nil)

			for _, s := range tt.setup {
				_ = c.Apply("job-1", s)
			}

			if err := c.Apply("job-1", tt.bad); err == nil {
				t.Errorf("Apply(%T): expected error, got nil", tt.bad)
			}
		})
	}
}

func TestMemoryStore_Apply_SetsExitCodeAndError(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	c.Commit("job-1", struct{}{}, nil)
	_ = c.Apply("job-1", Started{})
	_ = c.Apply("job-1", Exited{ExitCode: 1})

	entry, _ := c.Get("job-1")
	if entry.State != StateFailed {
		t.Errorf("State: want %s, got %s", StateFailed, entry.State)
	}
	if entry.ExitCode == nil || *entry.ExitCode != 1 {
		t.Errorf("ExitCode: want 1, got %v", entry.ExitCode)
	}
}

func TestMemoryStore_Apply_AfterRelease(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	c.Commit("job-1", struct{}{}, nil)
	c.Release("job-1")

	if err := c.Apply("job-1", Started{}); err == nil {
		t.Error("Apply after Release: expected error, got nil")
	}
}

func TestMemoryStore_Apply_LogLine_NoTransition(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	c.Commit("job-1", struct{}{}, nil)

	if err := c.Apply("job-1", LogLine{Stream: "stdout", Lines: []string{"hello"}}); err != nil {
		t.Fatalf("Apply(LogLine): unexpected error: %v", err)
	}

	entry, _ := c.Get("job-1")
	if entry.State != StateAccepted {
		t.Errorf("State: want %s (no change), got %s", StateAccepted, entry.State)
	}
}

func TestMemoryStore_Release_ConcurrentSafe(t *testing.T) {
	c := newTestController()
	_ = c.Reserve("job-1")
	c.Commit("job-1", struct{}{}, nil)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Apply("job-1", Started{})
		}()
	}
	wg.Wait()
}
