package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"orchestrator/internal/circuitbreaker"
	"orchestrator/internal/cloudevent"
	"sync"
	"sync/atomic"
	"time"
)

// Memory is an in-memory async event dispatcher.
// Events are queued in a bounded channel and delivered by a worker pool.
// If the buffer is full, events are dropped (logged + metric incremented).
type Memory struct {
	queue    chan *Event
	chain    DeliveryFunc
	breakers *circuitbreaker.Registry
	config   Config
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
	isClosed atomic.Bool
}

// MetricsRecorder is an optional interface for recording dispatcher metrics.
type MetricsRecorder interface {
	RecordDispatcherDelivered(ctx context.Context, durationSeconds float64)
	RecordDispatcherFailed(ctx context.Context)
	RecordDispatcherDropped(ctx context.Context)
	RecordDispatcherRequeued(ctx context.Context)
}

// NewMemory creates a new in-memory dispatcher.
func NewMemory(cfg Config, metrics MetricsRecorder) *Memory {
	cfg = cfg.withDefaults()

	breakers := circuitbreaker.NewRegistry(circuitbreaker.Config{
		Threshold: defaultBreakerThreshold,
		Cooldown:  cfg.BreakerCooldown,
	})

	d := &Memory{
		queue:    make(chan *Event, cfg.BufferSize),
		breakers: breakers,
		config:   cfg,
		logger:   slog.With("component", "dispatcher"),
		metrics:  metrics,
		shutdown: make(chan struct{}),
	}

	// Assemble the delivery chain: circuit breaker → retry → HTTP send.
	// The onRetry callback increments the retry counter on d.
	d.chain = WithCircuitBreaker(
		WithRetry(
			HTTPSender(cloudevent.NewSender(cfg.HTTPTimeout)),
			defaultMaxRetries,
			nil,
			func() { d.retriesTotal.Add(1) },
		),
		breakers,
	)

	// Start workers
	d.wg.Add(cfg.Workers)
	for range cfg.Workers {
		go d.worker()
	}

	d.logger.Info("Dispatcher started", "workers", cfg.Workers, "buffer", cfg.BufferSize)
	return d
}

// QueueSize returns the number of events waiting for a worker. Read at scrape
// time by the dispatcher_queue_size async gauge.
func (d *Memory) QueueSize() int64 {
	return int64(len(d.queue))
}

// Dispatch queues an event for async delivery.
func (d *Memory) Dispatch(event *Event) error {
	if d.isClosed.Load() {
		return errors.New("dispatcher is closed")
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
func (d *Memory) Stats() Stats {
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
func (d *Memory) Close(ctx context.Context) error {
	if d.isClosed.Swap(true) {
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
func (d *Memory) worker() {
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
func (d *Memory) drainQueue() {
	for {
		select {
		case event := <-d.queue:
			d.deliver(event)
		default:
			return // queue empty
		}
	}
}

// deliver runs the event through the delivery chain and handles the outcome.
func (d *Memory) deliver(event *Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := d.chain(ctx, event)

	switch {
	case err == nil:
		d.delivered.Add(1)
		if d.metrics != nil {
			d.metrics.RecordDispatcherDelivered(ctx, time.Since(start).Seconds())
		}
	case errors.Is(err, ErrCircuitOpen):
		d.requeue(event, extractHost(event.Destination))
	default:
		d.failed.Add(1)
		if d.metrics != nil {
			d.metrics.RecordDispatcherFailed(ctx)
		}
		d.logger.Warn("Delivery failed",
			"destination", extractHost(event.Destination),
			"type", event.Payload.Type,
			"error", err,
		)
	}
}

// requeue puts an event back in the queue after a delay when circuit is open.
func (d *Memory) requeue(event *Event, host string) {
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

	// Clone the event with an incremented requeue count so the goroutine below
	// owns its own copy and workers never race on the Requeues field.
	next := *event
	next.Requeues++
	d.requeued.Add(1)
	if d.metrics != nil {
		d.metrics.RecordDispatcherRequeued(context.Background())
	}

	// Requeue after cooldown period so circuit has time to recover
	go func() {
		select {
		case <-d.shutdown:
			return
		case <-time.After(d.config.BreakerCooldown):
		}

		select {
		case d.queue <- &next:
			d.logger.Debug("Event requeued", "destination", host, "type", next.Payload.Type, "requeues", next.Requeues)
		case <-d.shutdown:
		default:
			// Buffer full, drop
			d.dropped.Add(1)
			if d.metrics != nil {
				d.metrics.RecordDispatcherDropped(context.Background())
			}
			d.logger.Warn("Event dropped on requeue, buffer full", "destination", host, "type", next.Payload.Type)
		}
	}()
}

// extractHost extracts the host from a URL for circuit breaker keying.
func extractHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	return parsed.Host
}

// Verify Memory implements Queue
var _ Queue = (*Memory)(nil)
