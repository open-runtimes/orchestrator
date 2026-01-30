// Package dispatcher provides async event dispatch with buffering and retry.
package dispatcher

import (
	"context"
	"errors"
	"orchestrator/pkg/cloudevent"
)

// ErrBufferFull is returned when the dispatcher's buffer is full and the event is dropped.
var ErrBufferFull = errors.New("dispatcher buffer full, event dropped")

// Dispatcher handles async delivery of events.
// Implementations may use in-memory buffering, message queues, etc.
type Dispatcher interface {
	// Dispatch queues an event for async delivery. Non-blocking.
	// Returns ErrBufferFull if the event cannot be queued.
	Dispatch(event *Event) error

	// Stats returns current dispatcher statistics.
	Stats() Stats

	// Close gracefully shuts down, attempting to deliver queued events.
	// The context deadline controls how long to wait for drain.
	Close(ctx context.Context) error
}

// Event is an event to be delivered to a destination.
type Event struct {
	Payload     *cloudevent.CloudEvent
	Destination string // callback URL
	SigningKey  string // HMAC key for signing, empty = no signing
	Signature   string // Pre-computed signature, takes precedence over SigningKey
	Requeues    int    // number of times requeued due to circuit open (internal use)
}

// Stats holds dispatcher statistics.
type Stats struct {
	QueueDepth    int   // current queue size
	Queued        int64 // total events queued
	Delivered     int64 // successful deliveries
	Failed        int64 // failed after retries
	Dropped       int64 // dropped due to full buffer or max requeues
	Requeued      int64 // requeued due to open circuit
	RetriesTotal  int64 // total retry attempts
	BreakersTotal int   // total circuit breakers
	BreakersOpen  int   // currently open breakers
}
