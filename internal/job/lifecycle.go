package job

import "orchestrator/internal/cloudevent"

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
//
// dest is nil when the job has no callback. Exit events are emitted anyway,
// with an empty destination, so the metrics listener still sees every exit.
func EmitCallback(em *CallbackEmitter, jobID, image string, dest *CallbackDest, s Signal) {
	if dest == nil {
		if _, exited := s.(Exited); !exited {
			if _, failed := s.(Failed); !failed {
				return
			}
		}
		dest = &CallbackDest{}
	}
	var event *cloudevent.Event
	switch ev := s.(type) {
	case Started:
		event = StartEvent(jobID, dest.Meta)
	case Exited:
		event = ExitEvent(jobID, dest.Meta, ev.ExitCode, ev.Reason, image, ev.Duration.Seconds())
	case Failed:
		event = ExitEvent(jobID, dest.Meta, -1, ev.Reason, image, 0)
	case Completed:
		event = CompleteEvent(jobID, dest.Meta)
	case LogLine:
		event = LogEvent(jobID, dest.Meta, ev.Lines, ev.Stream)
	default:
		return
	}
	if !MatchesCallbackFilter(event.Type, dest.Events) {
		return
	}
	em.Emit(&CallbackEnvelope{Payload: event, CallbackURL: dest.URL, SigningKey: dest.Key})
}

// EmitArtifactCallback dispatches a sidecar's artifact report as a callback.
// It is a no-op when the job has no callback or filters the artifact event out.
func EmitArtifactCallback(em *CallbackEmitter, r ArtifactReport) {
	if r.CallbackURL == "" || !MatchesCallbackFilter(CallbackTypeArtifact, r.CallbackEvents) {
		return
	}
	em.Emit(&CallbackEnvelope{Payload: ArtifactEvent(&r), CallbackURL: r.CallbackURL, SigningKey: r.CallbackKey})
}
