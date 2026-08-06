package job

import (
	"orchestrator/internal/cloudevent"
	"testing"
)

func TestEventEmitter_NoListeners(t *testing.T) {
	t.Parallel()
	emitter := NewCallbackEmitter()

	// Should not panic
	emitter.Emit(&CallbackEnvelope{
		Payload:     cloudevent.New("test.event", "src", "sub", "id", nil),
		CallbackURL: "http://example.com",
	})
}

func TestEventEmitter_SingleListener(t *testing.T) {
	t.Parallel()
	emitter := NewCallbackEmitter()

	var received []*CallbackEnvelope
	emitter.Register(func(e *CallbackEnvelope) { received = append(received, e) })

	event := &CallbackEnvelope{
		Payload:     cloudevent.New("test.event", "src", "job-1", "id", nil),
		CallbackURL: "http://example.com/webhook",
		SigningKey:  "secret",
	}
	emitter.Emit(event)

	if len(received) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(received))
	}
	if received[0].CallbackURL != "http://example.com/webhook" {
		t.Errorf("Expected callback URL http://example.com/webhook, got %s", received[0].CallbackURL)
	}
	if received[0].SigningKey != "secret" {
		t.Errorf("Expected signing key 'secret', got %s", received[0].SigningKey)
	}
	if received[0].Payload.Type != "test.event" {
		t.Errorf("Expected event type test.event, got %s", received[0].Payload.Type)
	}
}

func TestEventEmitter_MultipleListeners(t *testing.T) {
	t.Parallel()
	emitter := NewCallbackEmitter()

	var a, b []*CallbackEnvelope
	emitter.Register(func(e *CallbackEnvelope) { a = append(a, e) })
	emitter.Register(func(e *CallbackEnvelope) { b = append(b, e) })

	emitter.Emit(&CallbackEnvelope{
		Payload:     cloudevent.New("test.first", "src", "sub", "id1", nil),
		CallbackURL: "http://a.com",
	})
	emitter.Emit(&CallbackEnvelope{
		Payload:     cloudevent.New("test.second", "src", "sub", "id2", nil),
		CallbackURL: "http://b.com",
	})

	if len(a) != 2 {
		t.Errorf("Listener A: expected 2 events, got %d", len(a))
	}
	if len(b) != 2 {
		t.Errorf("Listener B: expected 2 events, got %d", len(b))
	}
}
