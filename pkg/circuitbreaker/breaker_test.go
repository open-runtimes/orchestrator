package circuitbreaker

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()

	if cfg.Threshold != 5 {
		t.Errorf("Expected Threshold 5, got %d", cfg.Threshold)
	}
	if cfg.Cooldown != 30*time.Second {
		t.Errorf("Expected Cooldown 30s, got %v", cfg.Cooldown)
	}
}

func TestNew_WithZeroValues(t *testing.T) {
	t.Parallel()
	// Zero values should use defaults
	b := New(Config{Threshold: 0, Cooldown: 0})

	// Verify defaults were applied by checking behavior
	// With default threshold of 5, should need 5 failures to open
	for i := 0; i < 4; i++ {
		b.RecordFailure()
	}
	if b.State() != Closed {
		t.Error("Expected closed state after 4 failures (default threshold is 5)")
	}

	b.RecordFailure()
	if b.State() != Open {
		t.Error("Expected open state after 5 failures")
	}
}

func TestNew_WithNegativeValues(t *testing.T) {
	t.Parallel()
	// Negative values should use defaults
	b := New(Config{Threshold: -1, Cooldown: -1})

	// Should use default threshold of 5
	for i := 0; i < 5; i++ {
		b.RecordFailure()
	}
	if b.State() != Open {
		t.Error("Expected open state after threshold failures")
	}
}

func TestBreaker_ClosedState(t *testing.T) {
	t.Parallel()
	b := New(Config{Threshold: 3, Cooldown: 100 * time.Millisecond})

	// Should allow requests in closed state
	if !b.Allow() {
		t.Error("expected Allow() to return true in closed state")
	}

	// Recording success should keep it closed
	b.RecordSuccess()
	if b.State() != Closed {
		t.Errorf("expected closed state, got %s", b.State())
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	t.Parallel()
	b := New(Config{Threshold: 3, Cooldown: 100 * time.Millisecond})

	// Record failures up to threshold
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Closed {
		t.Error("expected closed state before threshold")
	}

	// Third failure should open the circuit
	b.RecordFailure()
	if b.State() != Open {
		t.Errorf("expected open state after threshold, got %s", b.State())
	}

	// Should not allow requests when open
	if b.Allow() {
		t.Error("expected Allow() to return false when open")
	}
}

func TestBreaker_HalfOpenAfterCooldown(t *testing.T) {
	t.Parallel()
	b := New(Config{Threshold: 2, Cooldown: 50 * time.Millisecond})

	// Open the circuit
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Open {
		t.Fatal("expected open state")
	}

	// Should not allow before cooldown
	if b.Allow() {
		t.Error("expected Allow() to return false before cooldown")
	}

	// Wait for cooldown
	time.Sleep(60 * time.Millisecond)

	// Should allow one request (half-open)
	if !b.Allow() {
		t.Error("expected Allow() to return true after cooldown (half-open)")
	}
	if b.State() != HalfOpen {
		t.Errorf("expected half-open state, got %s", b.State())
	}
}

func TestBreaker_ClosesOnSuccessInHalfOpen(t *testing.T) {
	t.Parallel()
	b := New(Config{Threshold: 2, Cooldown: 10 * time.Millisecond})

	// Open the circuit
	b.RecordFailure()
	b.RecordFailure()

	// Wait for cooldown
	time.Sleep(15 * time.Millisecond)
	b.Allow() // Transition to half-open

	// Success should close the circuit
	b.RecordSuccess()
	if b.State() != Closed {
		t.Errorf("expected closed state after success, got %s", b.State())
	}
}

func TestBreaker_ReopensOnFailureInHalfOpen(t *testing.T) {
	t.Parallel()
	b := New(Config{Threshold: 2, Cooldown: 10 * time.Millisecond})

	// Open the circuit
	b.RecordFailure()
	b.RecordFailure()

	// Wait for cooldown
	time.Sleep(15 * time.Millisecond)
	b.Allow() // Transition to half-open

	// Failure should reopen
	b.RecordFailure()
	if b.State() != Open {
		t.Errorf("expected open state after failure in half-open, got %s", b.State())
	}
}

func TestBreaker_Reset(t *testing.T) {
	t.Parallel()
	b := New(Config{Threshold: 2, Cooldown: time.Second})

	// Open the circuit
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Open {
		t.Fatal("expected open state")
	}

	// Reset should close it
	b.Reset()
	if b.State() != Closed {
		t.Errorf("expected closed state after reset, got %s", b.State())
	}
	if b.Failures() != 0 {
		t.Errorf("expected 0 failures after reset, got %d", b.Failures())
	}
}

func TestBreaker_StateString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state    State
		expected string
	}{
		{Closed, "closed"},
		{Open, "open"},
		{HalfOpen, "half-open"},
		{State(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.expected)
		}
	}
}

func TestRegistry_GetCreatesBreaker(t *testing.T) {
	t.Parallel()
	r := NewRegistry(Config{Threshold: 5, Cooldown: time.Second})

	b1 := r.Get("service-a")
	b2 := r.Get("service-a")
	b3 := r.Get("service-b")

	// Same key should return same breaker
	if b1 != b2 {
		t.Error("expected same breaker for same key")
	}

	// Different key should return different breaker
	if b1 == b3 {
		t.Error("expected different breaker for different key")
	}

	stats := r.Stats()
	if stats.Total != 2 {
		t.Errorf("expected 2 breakers, got %d", stats.Total)
	}
}

func TestRegistry_Stats(t *testing.T) {
	t.Parallel()
	r := NewRegistry(Config{Threshold: 2, Cooldown: time.Second})

	// Create breakers in different states
	b1 := r.Get("service-a")
	b2 := r.Get("service-b")
	_ = r.Get("service-c") // stays closed

	// Open b1
	b1.RecordFailure()
	b1.RecordFailure()

	// Keep b2 closed
	_ = b2

	stats := r.Stats()
	if stats.Total != 3 {
		t.Errorf("expected 3 total, got %d", stats.Total)
	}
	if stats.Open != 1 {
		t.Errorf("expected 1 open, got %d", stats.Open)
	}
	if stats.Closed != 2 {
		t.Errorf("expected 2 closed, got %d", stats.Closed)
	}
}

func TestRegistry_Reset(t *testing.T) {
	t.Parallel()
	r := NewRegistry(Config{Threshold: 2, Cooldown: time.Second})

	b := r.Get("service-a")
	b.RecordFailure()
	b.RecordFailure()

	if b.State() != Open {
		t.Fatal("expected open state")
	}

	r.Reset()

	if b.State() != Closed {
		t.Errorf("expected closed after reset, got %s", b.State())
	}
}

func TestRegistry_Remove(t *testing.T) {
	t.Parallel()
	r := NewRegistry(Config{Threshold: 5, Cooldown: time.Second})

	_ = r.Get("service-a")
	_ = r.Get("service-b")

	r.Remove("service-a")

	keys := r.Keys()
	if len(keys) != 1 {
		t.Errorf("expected 1 key after remove, got %d", len(keys))
	}
}
