package job

import (
	"fmt"
	"time"
)

// Signal is the sealed set of backend-agnostic signals a LifecycleWatcher
// emits during a job's execution. Backends translate their native signals
// (Docker events, Kubernetes pod phases, etc.) into these types before sending.
type Signal interface {
	signal()
}

// Started is emitted when the worker container has started successfully.
type Started struct{}

// Exited is emitted when the worker container exits.
type Exited struct {
	ExitCode         int
	Duration         time.Duration
	FinalLogSequence uint64
}

// Failed is emitted when the job fails before or without the worker starting
// (e.g. sidecar crash, failure to start the worker container).
type Failed struct {
	Reason           string
	FinalLogSequence uint64
}

// LogLine is emitted for each batch of stdout/stderr lines from the worker.
type LogLine struct {
	Stream   string // "stdout" or "stderr"
	Lines    []string
	Sequence uint64
}

func (Started) signal() {}
func (Exited) signal()  {}
func (Failed) signal()  {}
func (LogLine) signal() {}

// CallbackDest holds the callback destination used when emitting lifecycle events.
// It is backend-agnostic: the Docker orchestrator builds it from container labels
// or a job.Request; a Kubernetes backend would build it the same way.
type CallbackDest struct {
	URL     string
	Key     string
	Events  []string
	Headers map[string]string
	Meta    map[string]string
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
				Headers:     dest.Headers,
			})
		}
	case Exited:
		emitExitCallback(em, jobID, image, dest, ev.ExitCode, ev.Duration.Seconds(), ev.FinalLogSequence)
	case Failed:
		emitExitCallback(em, jobID, image, dest, -1, 0, ev.FinalLogSequence)
	case LogLine:
		if dest == nil || !MatchesCallbackFilter(CallbackTypeLog, dest.Events) {
			return
		}
		builder := NewEventBuilder(jobID, "orchestrator/service", dest.Meta)
		em.Emit(&CallbackEnvelope{
			Payload:     builder.BuildLogEvent(ev.Lines, ev.Stream, ev.Sequence),
			CallbackURL: dest.URL,
			SigningKey:  dest.Key,
			Headers:     dest.Headers,
		})
	}
}

func emitExitCallback(em *CallbackEmitter, jobID, image string, dest *CallbackDest, exitCode int, durationSeconds float64, finalLogSequence uint64) {
	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("exit code %d", exitCode)
	}

	var callbackURL, signingKey string
	var eventFilter []string
	var meta map[string]string
	var headers map[string]string
	if dest != nil {
		meta = dest.Meta
		callbackURL = dest.URL
		signingKey = dest.Key
		eventFilter = dest.Events
		headers = dest.Headers
	}

	builder := NewEventBuilder(jobID, "orchestrator/service", meta)
	event := builder.BuildExitEvent(exitCode, image, durationSeconds, finalLogSequence, exitErr)
	if MatchesCallbackFilter(event.Type, eventFilter) {
		em.Emit(&CallbackEnvelope{
			Payload:     event,
			CallbackURL: callbackURL,
			SigningKey:  signingKey,
			Headers:     headers,
		})
	}
}
