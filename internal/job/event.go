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

const eventSource = "orchestrator/service"

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

// Payloads of the job callbacks, one per CloudEvent type. Meta is always
// present (null when the job set none) so a subscriber can read it unguarded.

type StartData struct {
	JobID string            `json:"jobId"`
	Meta  map[string]string `json:"meta"`
}

type CompleteData struct {
	JobID string            `json:"jobId"`
	Meta  map[string]string `json:"meta"`
}

type LogData struct {
	JobID  string            `json:"jobId"`
	Lines  []string          `json:"lines"`
	Stream string            `json:"stream"`
	Meta   map[string]string `json:"meta"`
}

type ExitData struct {
	JobID           string            `json:"jobId"`
	ExitCode        int               `json:"exitCode"`
	Image           string            `json:"image"`
	DurationSeconds float64           `json:"durationSeconds"`
	Meta            map[string]string `json:"meta"`
	Reason          string            `json:"reason,omitempty"`
	Error           *callback.Failure `json:"error,omitempty"`
}

// ArtifactData carries the whole ArtifactReport so that a field added there
// reaches callback subscribers as well as the artifact endpoint. Format and
// Compression are omitted rather than sent empty: absent means "could not be
// determined", which a subscriber can act on differently from a real value.
type ArtifactData struct {
	JobID           string            `json:"jobId"`
	ArtifactID      string            `json:"artifactId"`
	ArtifactType    string            `json:"artifactType"`
	Status          string            `json:"status"`
	DurationSeconds float64           `json:"durationSeconds"`
	Meta            map[string]string `json:"meta"`
	Content         any               `json:"content,omitempty"`
	Format          string            `json:"format,omitempty"`
	Compression     string            `json:"compression,omitempty"`
	Error           *callback.Failure `json:"error,omitempty"`
}

func newEvent(jobID, eventType string, data any) *cloudevent.Event {
	id := fmt.Sprintf("%s-%d", jobID, time.Now().UnixNano())
	return cloudevent.New(eventType, eventSource, jobID, id, data)
}

func StartEvent(jobID string, meta map[string]string) *cloudevent.Event {
	return newEvent(jobID, CallbackTypeStart, StartData{JobID: jobID, Meta: meta})
}

// CompleteEvent is emitted after post-job artifacts have been processed.
func CompleteEvent(jobID string, meta map[string]string) *cloudevent.Event {
	return newEvent(jobID, CallbackTypeComplete, CompleteData{JobID: jobID, Meta: meta})
}

func LogEvent(jobID string, meta map[string]string, lines []string, stream string) *cloudevent.Event {
	return newEvent(jobID, CallbackTypeLog, LogData{JobID: jobID, Lines: lines, Stream: stream, Meta: meta})
}

func ArtifactEvent(r *ArtifactReport) *cloudevent.Event {
	data := ArtifactData{
		JobID:           r.JobID,
		ArtifactID:      r.ID,
		ArtifactType:    r.Type,
		Status:          r.Status,
		DurationSeconds: r.DurationSeconds,
		Meta:            r.Meta,
		Content:         r.Content,
		Format:          r.Format,
		Compression:     r.Compression,
	}
	if r.Status == "failed" {
		code, message := artifact.CodeError(r.FailureReason), r.FailureMessage
		if !code.Valid() {
			// Old sidecars send prose in FailureReason. It was never meant for
			// the wire, so it is neither parsed for a code nor forwarded.
			code = artifact.FailureCode(r.Type, nil)
		}
		if message == "" {
			message = fmt.Sprintf("artifact %s failed", r.ID)
		}
		f := callback.Fail(string(code), message)
		data.Error = &f
	}
	return newEvent(r.JobID, CallbackTypeArtifact, data)
}

func ExitEvent(jobID string, meta map[string]string, exitCode int, reason, image string, durationSeconds float64) *cloudevent.Event {
	data := ExitData{
		JobID:           jobID,
		ExitCode:        exitCode,
		Image:           image,
		DurationSeconds: durationSeconds,
		Meta:            meta,
		Reason:          reason,
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
		f := callback.Fail(string(code), message)
		data.Error = &f
	}
	return newEvent(jobID, CallbackTypeExit, data)
}
