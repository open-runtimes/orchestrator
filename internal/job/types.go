package job

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/lifecycle"
	"orchestrator/internal/volume"
)

// Request represents a request to create a new job
type Request struct {
	ID             string            `json:"id"`
	Meta           map[string]string `json:"meta"`
	Image          string            `json:"image"`
	Command        string            `json:"command"`
	CPU            float64           `json:"cpu"`
	Memory         int               `json:"memory"`
	Environment    map[string]string `json:"environment"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Workspace      string            `json:"workspace,omitempty"` // Working directory and mount path (default: /workspace)
	Artifacts      artifact.Set      `json:"artifacts,omitempty"`
	Volumes        []volume.Volume   `json:"volumes,omitempty"` // existing Docker volumes / K8s PVCs mounted into the worker
	Callback       *Callback         `json:"callback,omitempty"`

	// ArtifactToken authenticates the sidecar's posts to the internal artifact
	// endpoint. Set by the Service, never by API clients — json:"-" keeps it
	// off the wire, so it can't round-trip through the API.
	ArtifactToken string `json:"-"`
}

// Parse decodes an API request body, rejecting unknown fields — a typo'd
// field name must fail loudly, not silently run with defaults. Strictness
// belongs at the API edge only: stored specs (labels, annotations) decode
// leniently so version skew never strands them.
func Parse(data []byte) (*Request, error) {
	var r Request
	if err := artifact.UnmarshalStrict(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Callback represents callback configuration for a job
type Callback struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
	Key    string   `json:"key,omitempty"` // HMAC signing key
}

// Response represents the response when a job is created
type Response struct {
	ID    string `json:"id"`
	State string `json:"status"` // "accepted"
}

// StatusResponse represents the current status of a job
type StatusResponse struct {
	ID       string `json:"id"`
	State    string `json:"status"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ListResponse represents the response for listing jobs
type ListResponse struct {
	Jobs []StatusResponse `json:"jobs"`
}

// ArtifactReport is posted by the sidecar to report the result of a single artifact operation.
// The orchestrator uses it to construct and dispatch the corresponding CloudEvent.
// Callback config is passed through from the sidecar so the orchestrator does not need to
// duplicate it in its own state.
type ArtifactReport struct {
	JobID         string `json:"jobId"`
	ID            string `json:"id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	Content       any    `json:"content,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`

	// What the artifact turned out to be, sniffed from its header. Two axes:
	// Format is the container, Compression the codec inside it. Empty when
	// the artifact was never read far enough to tell.
	Format      string `json:"format,omitempty"`
	Compression string `json:"compression,omitempty"`

	DurationSeconds float64 `json:"durationSeconds"`
	OutputBytes     int64   `json:"outputBytes,omitempty"`

	CallbackURL    string            `json:"callbackUrl,omitempty"`
	CallbackKey    string            `json:"callbackKey,omitempty"`
	CallbackEvents []string          `json:"callbackEvents,omitempty"`
	Meta           map[string]string `json:"meta,omitempty"`
}

// State constants, shared with pkg/lifecycle.
const (
	StateAccepted  = lifecycle.StateAccepted
	StateRunning   = lifecycle.StateRunning
	StateCompleted = lifecycle.StateCompleted
	StateFailed    = lifecycle.StateFailed
	StateCancelled = lifecycle.StateCancelled
)
