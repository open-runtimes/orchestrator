package job

import (
	"encoding/json"
	"fmt"
	"orchestrator/internal/artifact"
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
	Callback       *Callback         `json:"callback,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling for Request.
func (r *Request) UnmarshalJSON(data []byte) error {
	var raw requestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	r.ID = raw.ID
	r.Meta = raw.Meta
	r.Image = raw.Image
	r.Command = raw.Command
	r.CPU = raw.CPU
	r.Memory = raw.Memory
	r.Environment = raw.Environment
	r.TimeoutSeconds = raw.TimeoutSeconds
	r.Workspace = raw.Workspace
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
	ID     string `json:"id"`
	Status string `json:"status"` // "accepted"
}

// Status represents the current status of a job
type Status struct {
	ID       string `json:"id"`
	State    string `json:"status"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ListResponse represents the response for listing jobs
type ListResponse struct {
	Jobs []Status `json:"jobs"`
}

// State constants
const (
	StateAccepted  = "accepted"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)
