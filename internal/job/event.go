package job

import (
	"fmt"
	"orchestrator/internal/artifact"
	"orchestrator/internal/cloudevent"
	"slices"
	"time"
)

// Callback types (CloudEvent type strings) for job lifecycle
const (
	CallbackTypeStart    = "orchestrator.job.start"
	CallbackTypeArtifact = "orchestrator.job.artifact"
	CallbackTypeLog      = "orchestrator.job.log"
	CallbackTypeExit     = "orchestrator.job.exit"
	CallbackTypeComplete = "orchestrator.job.complete"
)

// ExitError is the error field of a failed exit callback. The process status
// remains in exitCode and the backend's existing detail remains in reason.
type ExitError string

const (
	ErrorExitNonzero ExitError = "job_exit_nonzero"
	ErrorFailed      ExitError = "job_failed"
	ErrorOOM         ExitError = "job_oom"
)

// MatchesCallbackFilter returns true if the event type should be sent based on the filter.
// If the filter is empty, all events are allowed.
func MatchesCallbackFilter(eventType string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	return slices.Contains(filter, eventType)
}

// EventBuilder builds CloudEvents for job lifecycle events.
type EventBuilder struct {
	source  string
	subject string
	meta    map[string]string
}

// NewEventBuilder creates a new EventBuilder.
func NewEventBuilder(jobID, source string, meta map[string]string) *EventBuilder {
	return &EventBuilder{
		source:  source,
		subject: jobID,
		meta:    meta,
	}
}

// Build creates a new CloudEvent with the given type and data.
func (b *EventBuilder) Build(eventType string, data map[string]any) *cloudevent.Event {
	eventID := fmt.Sprintf("%s-%d", b.subject, time.Now().UnixNano())
	return cloudevent.New(eventType, b.source, b.subject, eventID, data)
}

// BuildStartEvent creates a job start event.
func (b *EventBuilder) BuildStartEvent() *cloudevent.Event {
	data := map[string]any{
		"jobId": b.subject,
		"meta":  b.meta,
	}
	return b.Build(CallbackTypeStart, data)
}

// BuildArtifactEvent creates an artifact event.
//
// Takes the report whole so that a field added to ArtifactReport reaches
// callback subscribers as well as the artifact endpoint. The two are the same
// report seen by different consumers, and a positional signature let them
// drift: format and compression reached one and not the other.
func (b *EventBuilder) BuildArtifactEvent(r *ArtifactReport) *cloudevent.Event {
	data := map[string]any{
		"jobId":           b.subject,
		"artifactId":      r.ID,
		"artifactType":    r.Type,
		"status":          r.Status,
		"durationSeconds": r.DurationSeconds,
		"meta":            b.meta,
	}
	if r.Content != nil {
		data["content"] = r.Content
	}
	// Omitted rather than sent empty: absent means "could not be determined",
	// which a subscriber can act on differently from a real value.
	if r.Format != "" {
		data["format"] = r.Format
	}
	if r.Compression != "" {
		data["compression"] = r.Compression
	}
	if r.Status == "failed" {
		code := artifact.CodeError(r.FailureReason)
		if !code.Valid() {
			// Old sidecars send prose. Never leak it or guess a specific
			// cause from its wording during a rolling upgrade.
			code = artifact.FailureCode(r.Type, nil)
		}
		data["error"] = string(code)
	}
	return b.Build(CallbackTypeArtifact, data)
}

// BuildLogEvent creates a log event.
func (b *EventBuilder) BuildLogEvent(lines []string, stream string) *cloudevent.Event {
	data := map[string]any{
		"jobId":  b.subject,
		"lines":  lines,
		"stream": stream,
		"meta":   b.meta,
	}
	return b.Build(CallbackTypeLog, data)
}

// BuildCompleteEvent creates a job complete event, emitted after post-job
// artifacts have been processed.
func (b *EventBuilder) BuildCompleteEvent() *cloudevent.Event {
	data := map[string]any{
		"jobId": b.subject,
		"meta":  b.meta,
	}
	return b.Build(CallbackTypeComplete, data)
}

// BuildExitEvent creates an exit event.
func (b *EventBuilder) BuildExitEvent(exitCode int, reason, image string, durationSeconds float64, err error) *cloudevent.Event {
	data := map[string]any{
		"jobId":           b.subject,
		"exitCode":        exitCode,
		"image":           image,
		"durationSeconds": durationSeconds,
		"meta":            b.meta,
	}
	if reason != "" {
		data["reason"] = reason
	}
	if exitCode != 0 || err != nil {
		code := ErrorExitNonzero
		if exitCode == -1 {
			code = ErrorFailed
		}
		if reason == ExitReasonOOM {
			code = ErrorOOM
		}
		data["error"] = string(code)
	}
	return b.Build(CallbackTypeExit, data)
}
