package dispatcher

import (
	"log/slog"
	"orchestrator/internal/job"
)

// CallbackListener adapts a Dispatcher to the job.EventListener interface.
// It receives job lifecycle events and dispatches them to callback URLs.
type CallbackListener struct {
	dispatcher Dispatcher
	logger     *slog.Logger
}

// NewCallbackListener creates a new CallbackListener.
func NewCallbackListener(d Dispatcher) *CallbackListener {
	return &CallbackListener{
		dispatcher: d,
		logger:     slog.With("component", "callback-listener"),
	}
}

// OnJobEvent dispatches a job event to its callback URL.
func (l *CallbackListener) OnJobEvent(event *job.Event) {
	if event.CallbackURL == "" {
		return
	}
	if err := l.dispatcher.Dispatch(&Event{
		Payload:     event.Payload,
		Destination: event.CallbackURL,
		SigningKey:  event.SigningKey,
	}); err != nil {
		l.logger.Warn("Failed to dispatch job event",
			"type", event.Payload.Type,
			"error", err,
		)
	}
}

// Verify CallbackListener implements job.EventListener
var _ job.EventListener = (*CallbackListener)(nil)
