package job

import (
	"orchestrator/pkg/cloudevent"
	"testing"
)

type recordingListener struct {
	events []*Event
}

func (r *recordingListener) OnJobEvent(event *Event) {
	r.events = append(r.events, event)
}

func TestEventEmitter_NoListeners(t *testing.T) {
	t.Parallel()
	emitter := NewEventEmitter()

	// Should not panic
	emitter.Emit(&Event{
		Payload:     cloudevent.New("test.event", "src", "sub", "id", nil),
		CallbackURL: "http://example.com",
	})
}

func TestEventEmitter_SingleListener(t *testing.T) {
	t.Parallel()
	emitter := NewEventEmitter()
	listener := &recordingListener{}
	emitter.OnEvent(listener)

	event := &Event{
		Payload:     cloudevent.New("test.event", "src", "job-1", "id", nil),
		CallbackURL: "http://example.com/webhook",
		SigningKey:  "secret",
	}
	emitter.Emit(event)

	if len(listener.events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(listener.events))
	}
	if listener.events[0].CallbackURL != "http://example.com/webhook" {
		t.Errorf("Expected callback URL http://example.com/webhook, got %s", listener.events[0].CallbackURL)
	}
	if listener.events[0].SigningKey != "secret" {
		t.Errorf("Expected signing key 'secret', got %s", listener.events[0].SigningKey)
	}
	if listener.events[0].Payload.Type != "test.event" {
		t.Errorf("Expected event type test.event, got %s", listener.events[0].Payload.Type)
	}
}

func TestEventEmitter_MultipleListeners(t *testing.T) {
	t.Parallel()
	emitter := NewEventEmitter()
	a := &recordingListener{}
	b := &recordingListener{}
	emitter.OnEvent(a)
	emitter.OnEvent(b)

	emitter.Emit(&Event{
		Payload:     cloudevent.New("test.first", "src", "sub", "id1", nil),
		CallbackURL: "http://a.com",
	})
	emitter.Emit(&Event{
		Payload:     cloudevent.New("test.second", "src", "sub", "id2", nil),
		CallbackURL: "http://b.com",
	})

	if len(a.events) != 2 {
		t.Errorf("Listener A: expected 2 events, got %d", len(a.events))
	}
	if len(b.events) != 2 {
		t.Errorf("Listener B: expected 2 events, got %d", len(b.events))
	}
}
