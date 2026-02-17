package job

import (
	"testing"
)

func TestStore_Set_CreateNew(t *testing.T) {
	s := NewStore()

	if err := s.Set("job-1", StateAccepted); err != nil {
		t.Fatalf("unexpected error creating entry: %v", err)
	}

	entry, ok := s.Get("job-1")
	if !ok {
		t.Fatal("expected entry to exist")
	}
	if entry.State != StateAccepted {
		t.Errorf("expected state %s, got %s", StateAccepted, entry.State)
	}
	if entry.ID != "job-1" {
		t.Errorf("expected ID job-1, got %s", entry.ID)
	}
	if entry.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
	if entry.UpdatedAt.IsZero() {
		t.Error("expected UpdatedAt to be set")
	}
}

func TestStore_Set_ValidTransitions(t *testing.T) {
	transitions := []struct {
		from string
		to   string
	}{
		{StateAccepted, StateRunning},
		{StateRunning, StateCompleted},
		{StateRunning, StateFailed},
		{StateRunning, StateCancelled},
		{StateAccepted, StateFailed},
		{StateAccepted, StateCancelled},
	}

	for _, tt := range transitions {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			s := NewStore()
			if err := s.Set("job-1", tt.from); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := s.Set("job-1", tt.to); err != nil {
				t.Fatalf("unexpected error for %s -> %s: %v", tt.from, tt.to, err)
			}
			entry, _ := s.Get("job-1")
			if entry.State != tt.to {
				t.Errorf("expected state %s, got %s", tt.to, entry.State)
			}
		})
	}
}

func TestStore_Set_InvalidTransitions(t *testing.T) {
	transitions := []struct {
		from string
		to   string
	}{
		{StateCompleted, StateRunning},
		{StateAccepted, StateCompleted},
		{StateRunning, StateAccepted},
		{StateFailed, StateRunning},
		{StateCancelled, StateRunning},
	}

	for _, tt := range transitions {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			s := NewStore()
			if err := s.Set("job-1", tt.from); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := s.Set("job-1", tt.to); err == nil {
				t.Errorf("expected error for %s -> %s, got nil", tt.from, tt.to)
			}
		})
	}
}

func TestStore_Set_DuplicateSameState(t *testing.T) {
	s := NewStore()
	if err := s.Set("job-1", StateAccepted); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.Set("job-1", StateAccepted); err == nil {
		t.Error("expected error for duplicate Set with same state")
	}
}

func TestStore_Set_TerminalNoOutgoing(t *testing.T) {
	terminal := []string{StateCompleted, StateFailed, StateCancelled}
	targets := []string{StateAccepted, StateRunning, StateCompleted, StateFailed, StateCancelled}

	for _, from := range terminal {
		for _, to := range targets {
			t.Run(from+"->"+to, func(t *testing.T) {
				s := NewStore()
				_ = s.Set("job-1", from)
				if err := s.Set("job-1", to); err == nil {
					t.Errorf("expected error transitioning from terminal state %s to %s", from, to)
				}
			})
		}
	}
}

func TestStore_Set_WithExitCode(t *testing.T) {
	s := NewStore()
	_ = s.Set("job-1", StateAccepted)
	_ = s.Set("job-1", StateRunning)

	if err := s.Set("job-1", StateCompleted, WithExitCode(0)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry, _ := s.Get("job-1")
	if entry.ExitCode == nil || *entry.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %v", entry.ExitCode)
	}
}

func TestStore_Set_WithError(t *testing.T) {
	s := NewStore()
	_ = s.Set("job-1", StateAccepted)
	_ = s.Set("job-1", StateRunning)

	if err := s.Set("job-1", StateFailed, WithExitCode(1), WithError("oom killed")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry, _ := s.Get("job-1")
	if entry.Error != "oom killed" {
		t.Errorf("expected error 'oom killed', got %q", entry.Error)
	}
	if entry.ExitCode == nil || *entry.ExitCode != 1 {
		t.Errorf("expected exit code 1, got %v", entry.ExitCode)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	s := NewStore()
	_, ok := s.Get("nonexistent")
	if ok {
		t.Error("expected ok to be false for nonexistent job")
	}
}

func TestStore_Get_ReturnsCopy(t *testing.T) {
	s := NewStore()
	_ = s.Set("job-1", StateAccepted)

	entry, _ := s.Get("job-1")
	entry.State = "mutated"

	original, _ := s.Get("job-1")
	if original.State != StateAccepted {
		t.Error("Get should return a copy, not a reference")
	}
}

func TestStore_List_Empty(t *testing.T) {
	s := NewStore()
	entries := s.List()
	if len(entries) != 0 {
		t.Errorf("expected empty list, got %d entries", len(entries))
	}
}

func TestStore_List_Populated(t *testing.T) {
	s := NewStore()
	_ = s.Set("job-1", StateAccepted)
	_ = s.Set("job-2", StateRunning)

	entries := s.List()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID] = true
	}
	if !ids["job-1"] || !ids["job-2"] {
		t.Errorf("expected job-1 and job-2 in list, got %v", ids)
	}
}

func TestStore_Remove(t *testing.T) {
	s := NewStore()
	_ = s.Set("job-1", StateAccepted)

	if !s.Remove("job-1") {
		t.Error("expected Remove to return true for existing job")
	}
	if _, ok := s.Get("job-1"); ok {
		t.Error("expected job to be removed")
	}
}

func TestStore_Remove_NotFound(t *testing.T) {
	s := NewStore()
	if s.Remove("nonexistent") {
		t.Error("expected Remove to return false for nonexistent job")
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
		t.Errorf("expected ID job-1, got %s", status.ID)
	}
	if status.State != StateFailed {
		t.Errorf("expected state %s, got %s", StateFailed, status.State)
	}
	if status.ExitCode == nil || *status.ExitCode != 1 {
		t.Errorf("expected exit code 1, got %v", status.ExitCode)
	}
	if status.Error != "signal: killed" {
		t.Errorf("expected error 'signal: killed', got %q", status.Error)
	}

	// Verify ExitCode is a copy
	*status.ExitCode = 99
	if *entry.ExitCode != 1 {
		t.Error("Status() should return a copy of ExitCode")
	}
}

func TestEntry_Status_NoExitCode(t *testing.T) {
	entry := &Entry{
		ID:    "job-1",
		State: StateRunning,
	}

	status := entry.Status()
	if status.ExitCode != nil {
		t.Errorf("expected nil exit code, got %v", status.ExitCode)
	}
}
