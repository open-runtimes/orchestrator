package artifact

import (
	"bytes"
	"encoding/json"
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
