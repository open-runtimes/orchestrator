package artifact

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// UnmarshalStrict decodes JSON rejecting unknown fields — the API-edge
// decode shared by every request type whose custom UnmarshalJSON (needed
// for the artifact type discriminator) hides field names from a caller's
// DisallowUnknownFields. Stored specs use the lenient codecs instead.
func UnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Set is a spec's artifact slice: it carries the type round-trip (concrete
// types in, "type" discriminators out) on the field itself, so
// artifact-bearing specs need no hand-written codec of their own. Before this
// existed, every such spec kept a shadow struct plus a fromRaw/MarshalJSON
// pair, and each new field had to be threaded through all of them — a field
// missed there is dropped silently on the wire.
type Set []Artifact

// UnmarshalJSON decodes each artifact into its concrete type — a plain decode
// cannot land on the Artifact interface.
func (s *Set) UnmarshalJSON(data []byte) error {
	artifacts, err := UnmarshalArtifacts(data)
	if err != nil {
		return err
	}
	*s = artifacts
	return nil
}

// MarshalJSON stamps each artifact with its "type" discriminator.
func (s Set) MarshalJSON() ([]byte, error) {
	return MarshalArtifacts(s)
}

// UnmarshalArtifact unmarshals a JSON artifact into its concrete type, chosen
// by the "type" discriminator.
func UnmarshalArtifact(data []byte) (Artifact, error) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to determine artifact type: %w", err)
	}
	a := newArtifact(env.Type)
	if a == nil {
		return nil, fmt.Errorf("unknown artifact type: %q", env.Type)
	}
	if err := json.Unmarshal(data, a); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s artifact: %w", env.Type, err)
	}
	return a, nil
}

// UnmarshalArtifacts unmarshals a JSON array of artifacts.
func UnmarshalArtifacts(data []byte) ([]Artifact, error) {
	var rawArtifacts []json.RawMessage
	if err := json.Unmarshal(data, &rawArtifacts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal artifacts array: %w", err)
	}
	artifacts := make([]Artifact, 0, len(rawArtifacts))
	for i, raw := range rawArtifacts {
		a, err := UnmarshalArtifact(raw)
		if err != nil {
			return nil, fmt.Errorf("artifact[%d]: %w", i, err)
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, nil
}

// MarshalArtifact marshals an artifact with its "type" discriminator, which the
// concrete types do not carry as a field. The round trip through a map is the
// plain way to add one key to an encoded object.
func MarshalArtifact(a Artifact) ([]byte, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
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
