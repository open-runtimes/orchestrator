package job

import (
	"orchestrator/pkg/cloudevent"
	"orchestrator/pkg/emitter"
)

// Event represents a job lifecycle event emitted by the orchestrator.
type Event struct {
	Payload     *cloudevent.Event
	CallbackURL string
	SigningKey   string
}

// EventEmitter fans out job events to registered listeners.
type EventEmitter = emitter.Emitter[*Event]

// NewEventEmitter creates a new EventEmitter.
func NewEventEmitter() *EventEmitter {
	return &EventEmitter{}
}
