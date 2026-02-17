package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/job"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/cloudevent"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallbackListener_MapsFieldsToDispatcher(t *testing.T) {
	t.Parallel()

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
	defer d.Close(context.Background())

	listener := NewCallbackListener(d)
	listener.OnJobEvent(&job.Event{
		Payload:     cloudevent.New("orchestrator.job.start", "orchestrator/service", "job-1", "evt-1", nil),
		CallbackURL: server.URL,
		SigningKey:  "test-key",
	})

	testutil.MustWaitFor(t, func() bool {
		return received.Load() >= 1
	}, testutil.WithTimeout(5*time.Second), testutil.WithInterval(100*time.Millisecond))

	stats := d.Stats()
	if stats.Delivered != 1 {
		t.Errorf("Expected 1 delivered, got %d", stats.Delivered)
	}
}

func TestCallbackListener_MultipleEvents(t *testing.T) {
	t.Parallel()

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
	defer d.Close(context.Background())

	listener := NewCallbackListener(d)

	events := []string{"orchestrator.job.start", "orchestrator.job.log", "orchestrator.job.exit"}
	for i, eventType := range events {
		listener.OnJobEvent(&job.Event{
			Payload:     cloudevent.New(eventType, "orchestrator/service", "job-1", eventType+"-"+string(rune('0'+i)), nil),
			CallbackURL: server.URL,
		})
	}

	testutil.MustWaitFor(t, func() bool {
		return received.Load() >= 3
	}, testutil.WithTimeout(5*time.Second), testutil.WithInterval(100*time.Millisecond))

	stats := d.Stats()
	if stats.Delivered != 3 {
		t.Errorf("Expected 3 delivered, got %d", stats.Delivered)
	}
}

func TestCallbackListener_WithMetrics(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	metrics := &mockMetrics{}
	d := NewMemory(MemoryConfig{
		BufferSize:  100,
		Workers:     2,
		HTTPTimeout: 5 * time.Second,
	}, metrics)
	defer d.Close(context.Background())

	listener := NewCallbackListener(d)
	listener.OnJobEvent(&job.Event{
		Payload:     cloudevent.New("orchestrator.job.exit", "orchestrator/service", "job-1", "evt-1", nil),
		CallbackURL: server.URL,
	})

	testutil.MustWaitFor(t, func() bool {
		return metrics.delivered.Load() >= 1
	}, testutil.WithTimeout(5*time.Second), testutil.WithInterval(100*time.Millisecond))

	if metrics.delivered.Load() != 1 {
		t.Errorf("Expected 1 delivered metric, got %d", metrics.delivered.Load())
	}
}

// mockMetrics implements MetricsRecorder for testing.
type mockMetrics struct {
	delivered atomic.Int64
	failed    atomic.Int64
	dropped   atomic.Int64
	requeued  atomic.Int64
}

func (m *mockMetrics) RecordDispatcherDelivered(_ context.Context, _ float64) {
	m.delivered.Add(1)
}

func (m *mockMetrics) RecordDispatcherFailed(_ context.Context) {
	m.failed.Add(1)
}

func (m *mockMetrics) RecordDispatcherDropped(_ context.Context) {
	m.dropped.Add(1)
}

func (m *mockMetrics) RecordDispatcherRequeued(_ context.Context) {
	m.requeued.Add(1)
}

func (m *mockMetrics) RecordDispatcherQueueSize(_ context.Context, _ int64) {}
