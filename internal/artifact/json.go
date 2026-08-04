package artifact

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// UnmarshalStrict decodes JSON rejecting unknown fields — the API-edge
// decode shared by every request type whose custom UnmarshalJSON (needed
// for registry-typed artifacts) hides field names from a caller's
// DisallowUnknownFields. Stored specs use the lenient codecs instead.
func UnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Set is a spec's artifact slice: it carries the registry round-trip
// (concrete types in, "type" discriminators out) on the field itself, so
// artifact-bearing specs need no hand-written codec of their own. Before this
// existed, every such spec kept a shadow struct plus a fromRaw/MarshalJSON
// pair, and each new field had to be threaded through all of them — a field
// missed there is dropped silently on the wire.
type Set []Artifact

// UnmarshalJSON decodes each artifact into its concrete type via the registry
// — a plain decode cannot land on the Artifact interface.
func (s *Set) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	artifacts, err := UnmarshalArtifacts(data)
	if err != nil {
		return fmt.Errorf("failed to unmarshal artifacts: %w", err)
	}
	*s = artifacts
	return nil
}

// MarshalJSON stamps each artifact with its "type" discriminator.
func (s Set) MarshalJSON() ([]byte, error) {
	return MarshalArtifacts(s)
}

// UnmarshalArtifact unmarshals a JSON artifact into the appropriate concrete type.
func UnmarshalArtifact(data []byte) (Artifact, error) {
	return DefaultRegistry().unmarshalOne(data)
}

// UnmarshalArtifacts unmarshals a JSON array of artifacts.
func UnmarshalArtifacts(data []byte) ([]Artifact, error) {
	return DefaultRegistry().Unmarshal(data)
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
