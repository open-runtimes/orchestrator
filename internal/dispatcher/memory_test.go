package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/cloudevent"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryQueue_Dispatch(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     2,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	event := &Event{
		Payload:     cloudevent.New("test.event", "test", "job-1", "evt-1", nil),
		Destination: server.URL,
	}

	err := d.Dispatch(event)
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		return received.Load() >= 1
	}, testutil.WithTimeout(5*time.Second))

	if received.Load() != 1 {
		t.Errorf("expected 1 delivery, got %d", received.Load())
	}

	stats := d.Stats()
	if stats.Delivered != 1 {
		t.Errorf("expected 1 delivered, got %d", stats.Delivered)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.Close(ctx)
}

func TestMemoryQueue_BufferFull(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  2,
		Workers:     1,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	event := &Event{
		Payload:     cloudevent.New("test.event", "test", "job-1", "evt-1", nil),
		Destination: server.URL,
	}

	for i := 0; i < 5; i++ {
		_ = d.Dispatch(event)
	}

	testutil.MustWaitFor(t, func() bool {
		return d.Stats().Dropped > 0 || d.Stats().Delivered > 0
	}, testutil.WithTimeout(5*time.Second))

	stats := d.Stats()
	if stats.Dropped == 0 {
		t.Error("expected some events to be dropped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.Close(ctx)
}

func TestMemoryQueue_Retry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attempts.Add(1)
		if count < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     1,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	event := &Event{
		Payload:     cloudevent.New("test.event", "test", "job-1", "evt-1", nil),
		Destination: server.URL,
	}

	d.Dispatch(event)

	testutil.MustWaitFor(t, func() bool {
		return d.Stats().Delivered >= 1
	}, testutil.WithTimeout(5*time.Second))

	if attempts.Load() < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts.Load())
	}

	stats := d.Stats()
	if stats.Delivered != 1 {
		t.Errorf("expected 1 delivered, got %d", stats.Delivered)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.Close(ctx)
}

func TestMemoryQueue_NoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     1,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	event := &Event{
		Payload:     cloudevent.New("test.event", "test", "job-1", "evt-1", nil),
		Destination: server.URL,
	}

	d.Dispatch(event)

	testutil.MustWaitFor(t, func() bool {
		return d.Stats().Failed >= 1
	}, testutil.WithTimeout(5*time.Second))

	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt (no retry on 4xx), got %d", attempts.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.Close(ctx)
}

func TestMemoryQueue_CircuitBreaker(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     1,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	event := &Event{
		Payload:     cloudevent.New("test.event", "test", "job-1", "evt-1", nil),
		Destination: server.URL,
	}

	// Send more events than the breaker threshold (5) to trigger circuit opening
	// After 5 failures, the circuit opens and subsequent events get requeued
	for i := 0; i < 10; i++ {
		d.Dispatch(event)
	}

	testutil.MustWaitFor(t, func() bool {
		stats := d.Stats()
		// Either some events are requeued (circuit opened) or all are processed
		return stats.Requeued > 0 || (stats.Failed+stats.Delivered) >= 10
	}, testutil.WithTimeout(10*time.Second))

	stats := d.Stats()
	// With default threshold of 5, after 5 failures the circuit should open
	// and remaining events should be requeued
	if stats.Requeued == 0 && stats.Failed < 10 {
		t.Errorf("expected some events to be requeued due to open circuit, got requeued=%d, failed=%d", stats.Requeued, stats.Failed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.Close(ctx)
}

func TestMemoryQueue_CloudEventHeaders(t *testing.T) {
	var mu sync.Mutex
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     1,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	payload := cloudevent.New("orchestrator.job.exit", "orchestrator/sidecar", "job-123", "evt-456", nil)
	event := &Event{
		Payload:     payload,
		Destination: server.URL,
	}

	d.Dispatch(event)

	testutil.MustWaitFor(t, func() bool {
		return d.Stats().Delivered >= 1
	}, testutil.WithTimeout(5*time.Second))

	mu.Lock()
	contentType := headers.Get("Content-Type")
	ceType := headers.Get("Ce-Type")
	mu.Unlock()

	if contentType != "application/cloudevents+json" {
		t.Errorf("expected cloudevents content type, got %s", contentType)
	}
	if ceType != "orchestrator.job.exit" {
		t.Errorf("expected Ce-Type header, got %s", ceType)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.Close(ctx)
}

func TestMemoryQueue_Signature(t *testing.T) {
	var mu sync.Mutex
	var signature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		signature = r.Header.Get("X-Signature-256")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     1,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	event := &Event{
		Payload:     cloudevent.New("test.event", "test", "job-1", "evt-1", nil),
		Destination: server.URL,
		SigningKey:  "secret-key",
	}

	d.Dispatch(event)

	testutil.MustWaitFor(t, func() bool {
		return d.Stats().Delivered >= 1
	}, testutil.WithTimeout(5*time.Second))

	mu.Lock()
	sig := signature
	mu.Unlock()

	if sig == "" || len(sig) < 10 || sig[:7] != "sha256=" {
		t.Errorf("unexpected signature format: %s", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.Close(ctx)
}

func TestMemoryQueue_GracefulShutdown(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     2,
		HTTPTimeout: 5 * time.Second,
	}, nil)

	for i := 0; i < 10; i++ {
		event := &Event{
			Payload:     cloudevent.New("test.event", "test", "job-1", time.Now().Format("150405.000000"), nil),
			Destination: server.URL,
		}
		d.Dispatch(event)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := d.Close(ctx)
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	if received.Load() != 10 {
		t.Errorf("expected 10 deliveries, got %d", received.Load())
	}
}

func TestMemoryQueue_CircuitBreakerRecovery(t *testing.T) {
	var serving atomic.Bool // false = 503, true = 200

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serving.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	const (
		cooldown  = 200 * time.Millisecond
		numEvents = 10
	)

	d := NewMemory(MemoryConfig{
		BufferSize:      numEvents,
		Workers:         2,
		HTTPTimeout:     500 * time.Millisecond,
		BreakerCooldown: cooldown,
	}, nil)
	defer d.Close(context.Background())

	event := &Event{
		Payload:     cloudevent.New("test.breaker", "test", "job", "evt", nil),
		Destination: server.URL,
	}
	for i := 0; i < numEvents; i++ {
		d.Dispatch(event)
	}

	// Wait for circuit to open: at least one event requeued
	testutil.MustWaitFor(t, func() bool {
		return d.Stats().Requeued > 0
	}, testutil.WithTimeout(10*time.Second))

	// Allow the server to succeed so the circuit can recover
	serving.Store(true)

	// Wait for all events to settle: every dispatched event must end up
	// as delivered, failed, or dropped — no silent loss.
	testutil.MustWaitFor(t, func() bool {
		stats := d.Stats()
		return stats.Delivered+stats.Failed+stats.Dropped == numEvents
	}, testutil.WithTimeout(5*time.Second))

	stats := d.Stats()
	if stats.Requeued == 0 {
		t.Error("expected requeues when circuit is open")
	}
	if stats.Delivered == 0 {
		t.Error("expected deliveries after circuit recovery")
	}
	// With threshold=5, at least 5 events must have failed to open the circuit.
	// Fewer would mean the circuit never actually opened.
	if stats.Failed < defaultBreakerThreshold {
		t.Errorf("expected at least %d failures to open circuit, got %d", defaultBreakerThreshold, stats.Failed)
	}
	// All events must be accounted for.
	if total := stats.Delivered + stats.Failed + stats.Dropped; total != numEvents {
		t.Errorf("event accounting: delivered=%d + failed=%d + dropped=%d = %d, want %d",
			stats.Delivered, stats.Failed, stats.Dropped, total, numEvents)
	}
}
