package artifact

import (
	"encoding/json"
	"fmt"
)

// envelope is used for initial JSON unmarshaling to determine the artifact type.
type envelope struct {
	Type string `json:"type"`
}

// UnmarshalArtifact unmarshals a JSON artifact into the appropriate concrete type.
func UnmarshalArtifact(data []byte) (Artifact, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to determine artifact type: %w", err)
	}

	var artifact Artifact
	switch env.Type {
	case "download":
		artifact = &Download{}
	case "upload":
		artifact = &Upload{}
	case "write":
		artifact = &Write{}
	case "read":
		artifact = &Read{}
	case "archive":
		artifact = &Archive{}
	case "unarchive":
		artifact = &Unarchive{}
	case "list":
		artifact = &List{}
	default:
		return nil, fmt.Errorf("unknown artifact type: %q", env.Type)
	}

	if err := json.Unmarshal(data, artifact); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s artifact: %w", env.Type, err)
	}

	return artifact, nil
}

// UnmarshalArtifacts unmarshals a JSON array of artifacts.
func UnmarshalArtifacts(data []byte) ([]Artifact, error) {
	var rawArtifacts []json.RawMessage
	if err := json.Unmarshal(data, &rawArtifacts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal artifacts array: %w", err)
	}

	artifacts := make([]Artifact, 0, len(rawArtifacts))
	for i, raw := range rawArtifacts {
		artifact, err := UnmarshalArtifact(raw)
		if err != nil {
			return nil, fmt.Errorf("artifact[%d]: %w", i, err)
		}
		artifacts = append(artifacts, artifact)
	}

	return artifacts, nil
}

// MarshalArtifact marshals an artifact with its type field included.
func MarshalArtifact(a Artifact) ([]byte, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}

	// Inject the type field
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m["type"] = a.ArtifactType()

	return json.Marshal(m)
}

// MarshalArtifacts marshals a slice of artifacts.
func MarshalArtifacts(artifacts []Artifact) ([]byte, error) {
	result := make([]json.RawMessage, 0, len(artifacts))
	for _, a := range artifacts {
		data, err := MarshalArtifact(a)
		if err != nil {
			return nil, err
		}
		result = append(result, data)
	}
	return json.Marshal(result)
}
