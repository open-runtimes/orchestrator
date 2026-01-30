package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"orchestrator/pkg/backoff"
	"orchestrator/pkg/circuitbreaker"
	"orchestrator/pkg/cloudevent"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryDispatcher is an in-memory async event dispatcher.
// Events are queued in a bounded channel and delivered by a worker pool.
// If the buffer is full, events are dropped (logged + metric incremented).
type MemoryDispatcher struct {
	queue    chan *Event
	sender   *cloudevent.Sender
	breakers *circuitbreaker.Registry
	config   MemoryConfig
	logger   *slog.Logger
	metrics  MetricsRecorder

	// Internal counters (for Stats())
	queued       atomic.Int64
	delivered    atomic.Int64
	failed       atomic.Int64
	dropped      atomic.Int64
	requeued     atomic.Int64
	retriesTotal atomic.Int64

	wg       sync.WaitGroup
	shutdown chan struct{}
	closed   atomic.Bool
}

// MetricsRecorder is an optional interface for recording dispatcher metrics.
type MetricsRecorder interface {
	RecordDispatcherDelivered(ctx context.Context, durationSeconds float64)
	RecordDispatcherFailed(ctx context.Context)
	RecordDispatcherDropped(ctx context.Context)
	RecordDispatcherRequeued(ctx context.Context)
	RecordDispatcherQueueSize(ctx context.Context, size int64)
}

// NewMemory creates a new in-memory dispatcher.
func NewMemory(cfg MemoryConfig, metrics MetricsRecorder) *MemoryDispatcher {
	cfg = cfg.withDefaults()

	d := &MemoryDispatcher{
		queue:  make(chan *Event, cfg.BufferSize),
		sender: cloudevent.NewSender(cfg.HTTPTimeout),
		breakers: circuitbreaker.NewRegistry(circuitbreaker.Config{
			Threshold: defaultBreakerThreshold,
			Cooldown:  defaultBreakerCooldown,
		}),
		config:   cfg,
		logger:   slog.With("component", "dispatcher"),
		metrics:  metrics,
		shutdown: make(chan struct{}),
	}

	// Start workers
	d.wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go d.worker()
	}

	// Start queue size reporter if metrics enabled
	if metrics != nil {
		go d.reportQueueSize()
	}

	d.logger.Info("Dispatcher started", "workers", cfg.Workers, "buffer", cfg.BufferSize)
	return d
}

// reportQueueSize periodically reports the queue size metric.
func (d *MemoryDispatcher) reportQueueSize() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.shutdown:
			return
		case <-ticker.C:
			d.metrics.RecordDispatcherQueueSize(context.Background(), int64(len(d.queue)))
		}
	}
}

// Dispatch queues an event for async delivery.
func (d *MemoryDispatcher) Dispatch(event *Event) error {
	if d.closed.Load() {
		return fmt.Errorf("dispatcher is closed")
	}

	select {
	case d.queue <- event:
		d.queued.Add(1)
		return nil
	default:
		d.dropped.Add(1)
		if d.metrics != nil {
			d.metrics.RecordDispatcherDropped(context.Background())
		}
		d.logger.Warn("Event dropped, buffer full",
			"destination", extractHost(event.Destination),
			"type", event.Payload.Type,
		)
		return ErrBufferFull
	}
}

// Stats returns current dispatcher statistics.
func (d *MemoryDispatcher) Stats() Stats {
	breakerStats := d.breakers.Stats()
	return Stats{
		QueueDepth:    len(d.queue),
		Queued:        d.queued.Load(),
		Delivered:     d.delivered.Load(),
		Failed:        d.failed.Load(),
		Dropped:       d.dropped.Load(),
		Requeued:      d.requeued.Load(),
		RetriesTotal:  d.retriesTotal.Load(),
		BreakersTotal: breakerStats.Total,
		BreakersOpen:  breakerStats.Open,
	}
}

// Close gracefully shuts down the dispatcher.
func (d *MemoryDispatcher) Close(ctx context.Context) error {
	if d.closed.Swap(true) {
		return nil // already closed
	}

	d.logger.Info("Dispatcher shutting down", "queued", len(d.queue))

	// Signal workers to stop
	close(d.shutdown)

	// Wait for workers with timeout
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		d.logger.Info("Dispatcher shutdown complete",
			"delivered", d.delivered.Load(),
			"failed", d.failed.Load(),
			"dropped", d.dropped.Load(),
		)
		return nil
	case <-ctx.Done():
		d.logger.Warn("Dispatcher shutdown timed out", "remaining", len(d.queue))
		return ctx.Err()
	}
}

// worker processes events from the queue.
func (d *MemoryDispatcher) worker() {
	defer d.wg.Done()

	for {
		select {
		case <-d.shutdown:
			// Drain remaining events before exiting
			d.drainQueue()
			return
		case event := <-d.queue:
			d.deliver(event)
		}
	}
}

// drainQueue delivers remaining events after shutdown signal.
func (d *MemoryDispatcher) drainQueue() {
	for {
		select {
		case event := <-d.queue:
			d.deliver(event)
		default:
			return // queue empty
		}
	}
}

// deliver attempts to deliver an event with retry and circuit breaker.
func (d *MemoryDispatcher) deliver(event *Event) {
	host := extractHost(event.Destination)
	breaker := d.breakers.Get(host)

	if !breaker.Allow() {
		d.requeue(event, host)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	if err := d.sendWithRetry(ctx, event); err != nil {
		breaker.RecordFailure()
		d.failed.Add(1)
		if d.metrics != nil {
			d.metrics.RecordDispatcherFailed(ctx)
		}
		d.logger.Warn("Delivery failed", "destination", host, "type", event.Payload.Type, "error", err)
		return
	}

	breaker.RecordSuccess()
	d.delivered.Add(1)
	if d.metrics != nil {
		d.metrics.RecordDispatcherDelivered(ctx, time.Since(start).Seconds())
	}
}

// requeue puts an event back in the queue after a delay when circuit is open.
func (d *MemoryDispatcher) requeue(event *Event, host string) {
	if event.Requeues >= defaultMaxRequeues {
		d.dropped.Add(1)
		if d.metrics != nil {
			d.metrics.RecordDispatcherDropped(context.Background())
		}
		d.logger.Warn("Event dropped, max requeues reached",
			"destination", host,
			"type", event.Payload.Type,
			"requeues", event.Requeues,
		)
		return
	}

	event.Requeues++
	requeues := event.Requeues // capture for goroutine
	d.requeued.Add(1)
	if d.metrics != nil {
		d.metrics.RecordDispatcherRequeued(context.Background())
	}

	// Requeue after cooldown period so circuit has time to recover
	go func() {
		select {
		case <-d.shutdown:
			return
		case <-time.After(defaultBreakerCooldown):
		}

		select {
		case d.queue <- event:
			d.logger.Debug("Event requeued", "destination", host, "type", event.Payload.Type, "requeues", requeues)
		case <-d.shutdown:
		default:
			// Buffer full, drop
			d.dropped.Add(1)
			if d.metrics != nil {
				d.metrics.RecordDispatcherDropped(context.Background())
			}
			d.logger.Warn("Event dropped on requeue, buffer full", "destination", host, "type", event.Payload.Type)
		}
	}()
}

func (d *MemoryDispatcher) sendWithRetry(ctx context.Context, event *Event) error {
	opts := cloudevent.SendOptions{
		SigningKey: event.SigningKey,
		Signature:  event.Signature,
	}

	var lastErr error
	for attempt := range defaultMaxRetries + 1 {
		if attempt > 0 {
			d.retriesTotal.Add(1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff.Exponential(attempt, nil)):
			}
		}

		lastErr = d.sender.Send(ctx, event.Destination, event.Payload, opts)
		if lastErr == nil {
			return nil
		}
		if cloudevent.IsClientError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// extractHost extracts the host from a URL for circuit breaker keying.
func extractHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	return parsed.Host
}

// Verify MemoryDispatcher implements Dispatcher
var _ Dispatcher = (*MemoryDispatcher)(nil)
