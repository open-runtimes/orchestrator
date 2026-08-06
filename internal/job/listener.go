package job

import (
	"orchestrator/internal/cloudevent"
	"orchestrator/internal/emitter"
)

// CallbackEnvelope represents an outbound callback emitted by the orchestrator.
type CallbackEnvelope struct {
	Payload     *cloudevent.Event
	CallbackURL string
	SigningKey  string
}

// CallbackEmitter fans out outbound callbacks to registered listeners.
type CallbackEmitter = emitter.Emitter[*CallbackEnvelope]

// NewCallbackEmitter creates a new CallbackEmitter.
func NewCallbackEmitter() *CallbackEmitter {
	return &CallbackEmitter{}
}
