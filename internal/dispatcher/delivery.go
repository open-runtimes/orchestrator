package dispatcher

import (
	"context"
	"errors"
	"orchestrator/internal/backoff"
	"orchestrator/internal/circuitbreaker"
	"orchestrator/internal/cloudevent"
	"time"
)

// DeliveryFunc is the composable handler type for event delivery.
// Middleware wraps a DeliveryFunc to add retry, circuit breaking, or other
// cross-cutting behaviour. The base of the chain performs the HTTP send.
type DeliveryFunc func(ctx context.Context, event *Event) error

// ErrCircuitOpen is returned by WithCircuitBreaker when the breaker for the
// destination host is open. Callers should requeue the event rather than
// count it as a permanent failure.
var ErrCircuitOpen = errors.New("circuit open")

// HTTPSender returns a DeliveryFunc that sends events over HTTP.
// It is the leaf of the delivery chain — the only layer that does I/O.
func HTTPSender(sender *cloudevent.Sender) DeliveryFunc {
	return func(ctx context.Context, event *Event) error {
		opts := cloudevent.SendOptions{
			SigningKey: event.SigningKey,
			Signature:  event.Signature,
		}
		return sender.Send(ctx, event.Destination, event.Payload, opts)
	}
}

// WithRetry wraps next with exponential-backoff retry for transient errors.
// Non-retryable errors (4xx client errors) are returned immediately.
// onRetry is called before each retry attempt and may be nil.
func WithRetry(next DeliveryFunc, maxRetries int, cfg *backoff.Config, onRetry func()) DeliveryFunc {
	return func(ctx context.Context, event *Event) error {
		var lastErr error
		for attempt := range maxRetries + 1 {
			if attempt > 0 {
				if onRetry != nil {
					onRetry()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(backoff.Exponential(attempt, cfg)):
				}
			}
			lastErr = next(ctx, event)
			if lastErr == nil {
				return nil
			}
			if cloudevent.IsClientError(lastErr) {
				return lastErr
			}
		}
		return lastErr
	}
}

// WithCircuitBreaker wraps next with per-host circuit breaking.
// When the circuit for the destination host is open, ErrCircuitOpen is
// returned immediately without calling next.
func WithCircuitBreaker(next DeliveryFunc, registry *circuitbreaker.Registry) DeliveryFunc {
	return func(ctx context.Context, event *Event) error {
		host := extractHost(event.Destination)
		breaker := registry.Get(host)

		if !breaker.Allow() {
			return ErrCircuitOpen
		}

		err := next(ctx, event)
		if err != nil {
			breaker.RecordFailure()
			return err
		}
		breaker.RecordSuccess()
		return nil
	}
}
