package job

import (
	"fmt"
	"orchestrator/pkg/cloudevent"
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
func (b *EventBuilder) BuildArtifactEvent(artifactID, artifactType, status string, content any, err error) *cloudevent.Event {
	data := map[string]any{
		"jobId":        b.subject,
		"artifactId":   artifactID,
		"artifactType": artifactType,
		"status":       status,
		"meta":         b.meta,
	}
	if content != nil {
		data["content"] = content
	}
	if err != nil {
		data["error"] = err.Error()
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
	if err != nil {
		data["error"] = err.Error()
	}
	return b.Build(CallbackTypeExit, data)
}
