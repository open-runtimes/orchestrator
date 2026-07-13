package job

import (
	"testing"
	"time"
)

func TestEmitCallback_Started_EmitsStartEvent(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Key: "secret"}, Started{})

	if len(captured) != 1 || captured[0].Payload.Type != CallbackTypeStart {
		t.Errorf("want start event, got %v", captured)
	}
}

func TestEmitCallback_Started_NilDest_NoEmit(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", nil, Started{})

	if len(captured) != 0 {
		t.Errorf("want no event with nil dest, got %v", captured)
	}
}

func TestEmitCallback_Started_FilteredOut_NoEmit(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Events: []string{CallbackTypeExit}}, Started{})

	if len(captured) != 0 {
		t.Errorf("want no event when filtered out, got %v", captured)
	}
}

func TestEmitCallback_Exited_EmitsExitEvent(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Key: "secret"}, Exited{ExitCode: 0, Duration: 2 * time.Second})

	if len(captured) != 1 || captured[0].Payload.Type != CallbackTypeExit {
		t.Errorf("want exit event, got %v", captured)
	}
}

func TestEmitCallback_Exited_NilDest_StillEmits(t *testing.T) {
	// Exit events are always emitted even without a callback dest (no URL though).
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", nil, Exited{ExitCode: 0})

	if len(captured) != 1 || captured[0].Payload.Type != CallbackTypeExit {
		t.Errorf("want exit event even with nil dest, got %v", captured)
	}
	if captured[0].CallbackURL != "" {
		t.Errorf("want empty CallbackURL for nil dest, got %q", captured[0].CallbackURL)
	}
}

func TestEmitCallback_Failed_EmitsExitWithNegativeCode(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Key: "secret"}, Failed{Reason: "sidecar died"})

	if len(captured) != 1 || captured[0].Payload.Data["exitCode"] != -1 {
		t.Errorf("want exit event with code -1, got %v", captured)
	}
}

func TestEmitCallback_Completed_EmitsCompleteEvent(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Key: "secret"}, Completed{})

	if len(captured) != 1 || captured[0].Payload.Type != CallbackTypeComplete {
		t.Errorf("want complete event, got %v", captured)
	}
}

func TestEmitCallback_Completed_FilteredOut_NoEmit(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Events: []string{CallbackTypeExit}}, Completed{})

	if len(captured) != 0 {
		t.Errorf("want no event when filtered out, got %v", captured)
	}
}

func TestEmitCallback_LogLine_EmitsLogEvent(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Key: "secret"}, LogLine{Stream: "stdout", Lines: []string{"hello"}})

	if len(captured) != 1 || captured[0].Payload.Type != CallbackTypeLog {
		t.Errorf("want log event, got %v", captured)
	}
}

func TestEmitCallback_LogLine_NilDest_NoEmit(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", nil, LogLine{Stream: "stdout", Lines: []string{"hello"}})

	if len(captured) != 0 {
		t.Errorf("want no event with nil dest, got %v", captured)
	}
}

func TestEmitCallback_CallbackURLAndKey_Propagated(t *testing.T) {
	em := NewCallbackEmitter()
	var captured []*CallbackEnvelope
	em.Register(func(e *CallbackEnvelope) { captured = append(captured, e) })

	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "https://example.com/cb", Key: "hmac-secret"}, Started{})

	if captured[0].CallbackURL != "https://example.com/cb" {
		t.Errorf("want CallbackURL propagated, got %q", captured[0].CallbackURL)
	}
	if captured[0].SigningKey != "hmac-secret" {
		t.Errorf("want SigningKey propagated, got %q", captured[0].SigningKey)
	}
}

func TestApplyThenEmitCallback_FSMUpdatedBeforeCallback(t *testing.T) {
	// Verify FSM state is Running when the start callback fires.
	store := NewMemoryStore[struct{}]()
	_ = store.Reserve("job-1")
	store.Commit("job-1", struct{}{}, nil)

	em := NewCallbackEmitter()
	var stateAtCallback string
	em.Register(func(e *CallbackEnvelope) {
		if e.Payload.Type == CallbackTypeStart {
			entry, _ := store.Get("job-1")
			stateAtCallback = entry.State
		}
	})

	_ = store.Apply("job-1", Started{})
	EmitCallback(em, "job-1", "alpine", &CallbackDest{URL: "http://example.com/cb", Key: "secret"}, Started{})

	if stateAtCallback != StateRunning {
		t.Errorf("want Running state when start callback fires, got %s", stateAtCallback)
	}
}
