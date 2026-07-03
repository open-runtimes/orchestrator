package job

import "fmt"

// CallbackDest holds the callback destination used when emitting lifecycle events.
// It is backend-agnostic: the Docker orchestrator builds it from container labels
// or a job.Request; a Kubernetes backend would build it the same way.
type CallbackDest struct {
	URL    string
	Key    string
	Events []string
	Meta   map[string]string
}

// EmitCallback translates a Signal into an outbound CloudEvent callback.
// FSM state must be updated (via Store.Apply) before calling this so that
// the callback reflects the new state.
func EmitCallback(em *CallbackEmitter, jobID, image string, dest *CallbackDest, s Signal) {
	switch ev := s.(type) {
	case Started:
		if dest == nil {
			return
		}
		builder := NewEventBuilder(jobID, "orchestrator/service", dest.Meta)
		event := builder.BuildStartEvent()
		if MatchesCallbackFilter(event.Type, dest.Events) {
			em.Emit(&CallbackEnvelope{
				Payload:     event,
				CallbackURL: dest.URL,
				SigningKey:  dest.Key,
			})
		}
	case Exited:
		emitExitCallback(em, jobID, image, dest, ev.ExitCode, ev.Duration.Seconds())
	case Failed:
		emitExitCallback(em, jobID, image, dest, -1, 0)
	case LogLine:
		if dest == nil || !MatchesCallbackFilter(CallbackTypeLog, dest.Events) {
			return
		}
		builder := NewEventBuilder(jobID, "orchestrator/service", dest.Meta)
		em.Emit(&CallbackEnvelope{
			Payload:     builder.BuildLogEvent(ev.Lines, ev.Stream),
			CallbackURL: dest.URL,
			SigningKey:  dest.Key,
		})
	}
}

func emitExitCallback(em *CallbackEmitter, jobID, image string, dest *CallbackDest, exitCode int, durationSeconds float64) {
	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("exit code %d", exitCode)
	}

	var callbackURL, signingKey string
	var eventFilter []string
	var meta map[string]string
	if dest != nil {
		meta = dest.Meta
		callbackURL = dest.URL
		signingKey = dest.Key
		eventFilter = dest.Events
	}

	builder := NewEventBuilder(jobID, "orchestrator/service", meta)
	event := builder.BuildExitEvent(exitCode, image, durationSeconds, exitErr)
	if MatchesCallbackFilter(event.Type, eventFilter) {
		em.Emit(&CallbackEnvelope{
			Payload:     event,
			CallbackURL: callbackURL,
			SigningKey:  signingKey,
		})
	}
}
