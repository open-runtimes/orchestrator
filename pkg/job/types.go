package job

import (
	"encoding/json"
	"fmt"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/lifecycle"
	"orchestrator/pkg/volume"
)

// Request represents a request to create a new job
type Request struct {
	ID             string              `json:"id"`
	Meta           map[string]string   `json:"meta"`
	Image          string              `json:"image"`
	Command        string              `json:"command"`
	CPU            float64             `json:"cpu"`
	Memory         int                 `json:"memory"`
	Environment    map[string]string   `json:"environment"`
	TimeoutSeconds int                 `json:"timeoutSeconds"`
	Workspace      string              `json:"workspace,omitempty"` // Working directory and mount path (default: /workspace)
	Artifacts      []artifact.Artifact `json:"artifacts,omitempty"`
	Volumes        []volume.Volume     `json:"volumes,omitempty"` // existing Docker volumes / K8s PVCs mounted into the worker
	Callback       *Callback           `json:"callback,omitempty"`
}

// requestJSON mirrors Request but with json.RawMessage for artifacts.
type requestJSON struct {
	ID             string            `json:"id"`
	Meta           map[string]string `json:"meta"`
	Image          string            `json:"image"`
	Command        string            `json:"command"`
	CPU            float64           `json:"cpu"`
	Memory         int               `json:"memory"`
	Environment    map[string]string `json:"environment"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Workspace      string            `json:"workspace,omitempty"`
	Artifacts      json.RawMessage   `json:"artifacts,omitempty"`
	Volumes        []volume.Volume   `json:"volumes,omitempty"`
	Callback       *Callback         `json:"callback,omitempty"`
}

// Parse decodes an API request body, rejecting unknown fields — a typo'd
// field name must fail loudly, not silently run with defaults. Strictness
// belongs at the API edge only: stored specs (labels, annotations) keep the
// lenient UnmarshalJSON so version skew never strands them.
func Parse(data []byte) (*Request, error) {
	var raw requestJSON
	if err := artifact.UnmarshalStrict(data, &raw); err != nil {
		return nil, err
	}
	var r Request
	if err := r.fromRaw(&raw); err != nil {
		return nil, err
	}
	return &r, nil
}

// UnmarshalJSON implements custom unmarshaling for Request.
func (r *Request) UnmarshalJSON(data []byte) error {
	var raw requestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	return r.fromRaw(&raw)
}

func (r *Request) fromRaw(raw *requestJSON) error {
	r.ID = raw.ID
	r.Meta = raw.Meta
	r.Image = raw.Image
	r.Command = raw.Command
	r.CPU = raw.CPU
	r.Memory = raw.Memory
	r.Environment = raw.Environment
	r.TimeoutSeconds = raw.TimeoutSeconds
	r.Workspace = raw.Workspace
	r.Volumes = raw.Volumes
	r.Callback = raw.Callback

	if len(raw.Artifacts) > 0 && string(raw.Artifacts) != "null" {
		artifacts, err := artifact.UnmarshalArtifacts(raw.Artifacts)
		if err != nil {
			return fmt.Errorf("failed to unmarshal artifacts: %w", err)
		}
		r.Artifacts = artifacts
	}

	return nil
}

// MarshalJSON implements custom marshaling for Request.
func (r Request) MarshalJSON() ([]byte, error) {
	raw := requestJSON{
		ID:             r.ID,
		Meta:           r.Meta,
		Image:          r.Image,
		Command:        r.Command,
		CPU:            r.CPU,
		Memory:         r.Memory,
		Environment:    r.Environment,
		TimeoutSeconds: r.TimeoutSeconds,
		Workspace:      r.Workspace,
		Volumes:        r.Volumes,
		Callback:       r.Callback,
	}

	if len(r.Artifacts) > 0 {
		artifactsData, err := artifact.MarshalArtifacts(r.Artifacts)
		if err != nil {
			return nil, err
		}
		raw.Artifacts = artifactsData
	}

	return json.Marshal(raw)
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
