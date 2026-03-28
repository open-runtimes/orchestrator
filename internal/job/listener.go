package job

import "orchestrator/pkg/cloudevent"

// Event represents a job lifecycle event emitted by the orchestrator.
type Event struct {
	Payload     *cloudevent.Event
	CallbackURL string
	SigningKey  string
}

// EventListener receives job lifecycle events.
type EventListener interface {
	OnJobEvent(event *Event)
}

// EventListenerFunc is a function that implements EventListener.
type EventListenerFunc func(event *Event)

func (f EventListenerFunc) OnJobEvent(event *Event) { f(event) }

// EventEmitter fans out job events to registered listeners.
type EventEmitter struct {
	listeners []EventListener
}

// NewEventEmitter creates a new EventEmitter.
func NewEventEmitter() *EventEmitter {
	return &EventEmitter{}
}

// OnEvent registers a listener. Must be called before the emitter is used.
func (e *EventEmitter) OnEvent(listener EventListener) {
	e.listeners = append(e.listeners, listener)
}

// Emit sends an event to all registered listeners.
func (e *EventEmitter) Emit(event *Event) {
	for _, l := range e.listeners {
		l.OnJobEvent(event)
	}
}
