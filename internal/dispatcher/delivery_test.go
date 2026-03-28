package dispatcher

import (
	"context"
	"errors"
	"net/http"
	"orchestrator/pkg/backoff"
	"orchestrator/pkg/circuitbreaker"
	"orchestrator/pkg/cloudevent"
	"sync/atomic"
	"testing"
	"time"
)

// fastBackoff eliminates wall-clock waits in retry tests.
var fastBackoff = &backoff.Config{Initial: time.Nanosecond, Max: time.Nanosecond}

func testDeliveryEvent() *Event {
	return &Event{
		Payload:     cloudevent.New("test.event", "test", "job-1", "evt-1", nil),
		Destination: "http://example.com/callback",
	}
}

// --- WithRetry ---

func TestWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		calls.Add(1)
		return nil
	})
	chain := WithRetry(next, 3, fastBackoff, nil)

	if err := chain(context.Background(), testDeliveryEvent()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

func TestWithRetry_RetriesOnTransientError(t *testing.T) {
	var calls atomic.Int32
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		if calls.Add(1) < 3 {
			return &cloudevent.HTTPError{StatusCode: http.StatusServiceUnavailable}
		}
		return nil
	})

	var retries atomic.Int32
	chain := WithRetry(next, 3, fastBackoff, func() { retries.Add(1) })

	if err := chain(context.Background(), testDeliveryEvent()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
	if retries.Load() != 2 {
		t.Errorf("expected 2 retries recorded, got %d", retries.Load())
	}
}

func TestWithRetry_NoRetryOnClientError(t *testing.T) {
	var calls atomic.Int32
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		calls.Add(1)
		return &cloudevent.HTTPError{StatusCode: http.StatusBadRequest}
	})
	chain := WithRetry(next, 3, fastBackoff, nil)

	if err := chain(context.Background(), testDeliveryEvent()); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (no retry on 4xx), got %d", calls.Load())
	}
}

func TestWithRetry_ExhaustsMaxRetries(t *testing.T) {
	var calls atomic.Int32
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		calls.Add(1)
		return &cloudevent.HTTPError{StatusCode: http.StatusServiceUnavailable}
	})
	chain := WithRetry(next, 3, fastBackoff, nil)

	if err := chain(context.Background(), testDeliveryEvent()); err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if calls.Load() != 4 { // 1 initial + 3 retries
		t.Errorf("expected 4 calls, got %d", calls.Load())
	}
}

func TestWithRetry_StopsOnContextCancellation(t *testing.T) {
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		return &cloudevent.HTTPError{StatusCode: http.StatusServiceUnavailable}
	})
	// Use real (tiny) backoff so the select fires between attempts
	chain := WithRetry(next, 10, &backoff.Config{Initial: 10 * time.Millisecond, Max: 10 * time.Millisecond}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := chain(ctx, testDeliveryEvent())
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// --- WithCircuitBreaker ---

func TestWithCircuitBreaker_AllowsWhenClosed(t *testing.T) {
	registry := circuitbreaker.NewRegistry(circuitbreaker.Config{Threshold: 5, Cooldown: time.Hour})
	var called bool
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		called = true
		return nil
	})
	chain := WithCircuitBreaker(next, registry)

	if err := chain(context.Background(), testDeliveryEvent()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected next to be called when circuit is closed")
	}
}

func TestWithCircuitBreaker_ReturnsErrCircuitOpenWhenOpen(t *testing.T) {
	const threshold = 3
	registry := circuitbreaker.NewRegistry(circuitbreaker.Config{Threshold: threshold, Cooldown: time.Hour})
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		return &cloudevent.HTTPError{StatusCode: http.StatusServiceUnavailable}
	})
	chain := WithCircuitBreaker(next, registry)

	for range threshold {
		chain(context.Background(), testDeliveryEvent()) //nolint:errcheck
	}

	err := chain(context.Background(), testDeliveryEvent())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestWithCircuitBreaker_DoesNotCallNextWhenOpen(t *testing.T) {
	registry := circuitbreaker.NewRegistry(circuitbreaker.Config{Threshold: 1, Cooldown: time.Hour})
	var calls atomic.Int32
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		calls.Add(1)
		return &cloudevent.HTTPError{StatusCode: http.StatusServiceUnavailable}
	})
	chain := WithCircuitBreaker(next, registry)

	chain(context.Background(), testDeliveryEvent()) //nolint:errcheck // trips the circuit
	chain(context.Background(), testDeliveryEvent()) //nolint:errcheck // should be blocked

	if calls.Load() != 1 {
		t.Errorf("expected next called once (not when open), got %d", calls.Load())
	}
}

func TestWithCircuitBreaker_RecordsFailureOnError(t *testing.T) {
	const threshold = 3
	registry := circuitbreaker.NewRegistry(circuitbreaker.Config{Threshold: threshold, Cooldown: time.Hour})
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		return &cloudevent.HTTPError{StatusCode: http.StatusInternalServerError}
	})
	chain := WithCircuitBreaker(next, registry)

	for range threshold - 1 {
		chain(context.Background(), testDeliveryEvent()) //nolint:errcheck
	}

	// One below threshold: circuit should still be closed
	if registry.Stats().Open != 0 {
		t.Error("expected circuit closed below threshold")
	}

	// Trip it
	chain(context.Background(), testDeliveryEvent()) //nolint:errcheck

	if registry.Stats().Open != 1 {
		t.Errorf("expected 1 open circuit after threshold failures, got %d", registry.Stats().Open)
	}
}

func TestWithCircuitBreaker_RecordsSuccessAndCloses(t *testing.T) {
	registry := circuitbreaker.NewRegistry(circuitbreaker.Config{Threshold: 1, Cooldown: time.Nanosecond})

	failing := true
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		if failing {
			return &cloudevent.HTTPError{StatusCode: http.StatusServiceUnavailable}
		}
		return nil
	})
	chain := WithCircuitBreaker(next, registry)

	chain(context.Background(), testDeliveryEvent()) //nolint:errcheck // open the circuit

	time.Sleep(2 * time.Nanosecond) // let cooldown expire → half-open

	failing = false
	if err := chain(context.Background(), testDeliveryEvent()); err != nil {
		t.Fatalf("expected success in half-open, got %v", err)
	}
	if registry.Stats().Open != 0 {
		t.Errorf("expected circuit closed after success, got %d open", registry.Stats().Open)
	}
}

func TestWithCircuitBreaker_PerHostIsolation(t *testing.T) {
	registry := circuitbreaker.NewRegistry(circuitbreaker.Config{Threshold: 1, Cooldown: time.Hour})
	next := DeliveryFunc(func(_ context.Context, _ *Event) error {
		return &cloudevent.HTTPError{StatusCode: http.StatusServiceUnavailable}
	})
	chain := WithCircuitBreaker(next, registry)

	hostA := &Event{Payload: cloudevent.New("t", "s", "j", "e", nil), Destination: "http://host-a/"}
	hostB := &Event{Payload: cloudevent.New("t", "s", "j", "e", nil), Destination: "http://host-b/"}

	chain(context.Background(), hostA) //nolint:errcheck // open circuit for host-a only

	// host-a should be open
	if !errors.Is(chain(context.Background(), hostA), ErrCircuitOpen) {
		t.Error("expected host-a circuit to be open")
	}
	// host-b should still be closed
	if errors.Is(chain(context.Background(), hostB), ErrCircuitOpen) {
		t.Error("expected host-b circuit to remain closed")
	}
}
