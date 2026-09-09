package job

import (
	"fmt"
	"orchestrator/internal/artifact"
	"orchestrator/internal/callback"
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

// ExitError is the code of a failed exit callback's error object. The process
// status remains in exitCode and the backend's own detail remains in reason.
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
		code, message := artifact.CodeError(r.FailureReason), r.FailureMessage
		if !code.Valid() {
			// Old sidecars send prose in FailureReason and no message. It is
			// detail, so it becomes the message — never a guess at the code.
			code, message = artifact.FailureCode(r.Type, nil), r.FailureReason
		}
		data["error"] = callback.Fail(string(code), message)
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
func (b *EventBuilder) BuildExitEvent(exitCode int, reason, image string, durationSeconds float64) *cloudevent.Event {
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
	// The message speaks of the job alone; backend vocabulary (pods, init
	// containers, sidecars) stays in reason, where it is documented as such.
	if exitCode != 0 {
		code, message := ErrorExitNonzero, fmt.Sprintf("job exited with code %d", exitCode)
		switch {
		case reason == ExitReasonOOM:
			code, message = ErrorOOM, "job was killed because it ran out of memory"
		case exitCode == -1:
			code, message = ErrorFailed, "job failed before it could start"
		}
		data["error"] = callback.Fail(string(code), message)
	}
	return b.Build(CallbackTypeExit, data)
}
