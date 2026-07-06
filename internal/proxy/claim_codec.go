package proxy

import (
	"encoding/json"
	"fmt"
	"orchestrator/internal/artifact"
)

// claimRequestJSON mirrors ClaimRequest with raw artifacts: the artifacts
// field is an interface slice, so it round-trips through the artifact
// registry — the same convention as pkg/job.Request.
type claimRequestJSON struct {
	ActivationID   string            `json:"activationId"`
	Command        string            `json:"command"`
	Environment    map[string]string `json:"environment,omitempty"`
	Artifacts      json.RawMessage   `json:"artifacts,omitempty"`
	Port           int               `json:"port,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
}

// UnmarshalJSON decodes artifacts into their concrete types via the registry.
func (c *ClaimRequest) UnmarshalJSON(data []byte) error {
	var raw claimRequestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ActivationID = raw.ActivationID
	c.Command = raw.Command
	c.Environment = raw.Environment
	c.Port = raw.Port
	c.TimeoutSeconds = raw.TimeoutSeconds
	if len(raw.Artifacts) > 0 && string(raw.Artifacts) != "null" {
		artifacts, err := artifact.UnmarshalArtifacts(raw.Artifacts)
		if err != nil {
			return fmt.Errorf("unmarshal artifacts: %w", err)
		}
		c.Artifacts = artifacts
	}
	return nil
}

// MarshalJSON encodes artifacts with their type tags so the wire form
// round-trips.
func (c ClaimRequest) MarshalJSON() ([]byte, error) {
	raw := claimRequestJSON{
		ActivationID:   c.ActivationID,
		Command:        c.Command,
		Environment:    c.Environment,
		Port:           c.Port,
		TimeoutSeconds: c.TimeoutSeconds,
	}
	if len(c.Artifacts) > 0 {
		data, err := artifact.MarshalArtifacts(c.Artifacts)
		if err != nil {
			return nil, err
		}
		raw.Artifacts = data
	}
	return json.Marshal(raw)
}
