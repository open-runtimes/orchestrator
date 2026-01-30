package backoff

import (
	"testing"
	"time"
)

func TestExponential_Defaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, 1600 * time.Millisecond},
		{6, 3200 * time.Millisecond},
		{7, 5 * time.Second}, // capped at max
		{8, 5 * time.Second}, // capped at max
	}

	for _, tt := range tests {
		got := Exponential(tt.attempt, nil)
		if got != tt.want {
			t.Errorf("Exponential(%d, nil) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestExponential_CustomConfig(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Initial: 50 * time.Millisecond,
		Max:     500 * time.Millisecond,
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 50 * time.Millisecond},
		{2, 100 * time.Millisecond},
		{3, 200 * time.Millisecond},
		{4, 400 * time.Millisecond},
		{5, 500 * time.Millisecond}, // capped at max
		{6, 500 * time.Millisecond}, // capped at max
	}

	for _, tt := range tests {
		got := Exponential(tt.attempt, cfg)
		if got != tt.want {
			t.Errorf("Exponential(%d, cfg) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestExponential_ZeroOrNegativeAttempt(t *testing.T) {
	t.Parallel()

	// Attempts < 1 should return initial
	if got := Exponential(0, nil); got != 100*time.Millisecond {
		t.Errorf("Exponential(0, nil) = %v, want 100ms", got)
	}
	if got := Exponential(-1, nil); got != 100*time.Millisecond {
		t.Errorf("Exponential(-1, nil) = %v, want 100ms", got)
	}
}

func TestExponential_PartialConfig(t *testing.T) {
	t.Parallel()

	// Only Initial set, Max uses default
	cfg := &Config{Initial: 200 * time.Millisecond}
	if got := Exponential(1, cfg); got != 200*time.Millisecond {
		t.Errorf("Exponential(1, {Initial: 200ms}) = %v, want 200ms", got)
	}
	if got := Exponential(6, cfg); got != 5*time.Second {
		t.Errorf("Exponential(6, {Initial: 200ms}) = %v, want 5s (default max)", got)
	}

	// Only Max set, Initial uses default
	cfg = &Config{Max: 300 * time.Millisecond}
	if got := Exponential(1, cfg); got != 100*time.Millisecond {
		t.Errorf("Exponential(1, {Max: 300ms}) = %v, want 100ms (default initial)", got)
	}
	if got := Exponential(3, cfg); got != 300*time.Millisecond {
		t.Errorf("Exponential(3, {Max: 300ms}) = %v, want 300ms (capped)", got)
	}
}
