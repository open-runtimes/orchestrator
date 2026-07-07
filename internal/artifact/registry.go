package artifact

import (
	"encoding/json"
	"fmt"
	"orchestrator/internal/apperrors"
	"sync"
)

// TypeDef describes a single artifact type to the registry.
// Register one of these per type; nothing else needs to change when adding a new type.
type TypeDef struct {
	// Type is the string that appears as "type" in JSON, e.g. "download".
	Type string
	// New returns a zero-value instance ready for json.Unmarshal.
	New func() Artifact
	// Validate checks field-level constraints. May be nil.
	Validate func(field string, a Artifact) error
	// SourcePath returns the filesystem path that must exist before Apply is called.
	// Return "" or leave nil to skip the file-wait.
	SourcePath func(a Artifact) string
}

// Registry holds a set of artifact type definitions. It is immutable after
// construction and safe for concurrent use without locking.
type Registry struct {
	types map[string]TypeDef
}

// NewRegistry builds a Registry from the provided TypeDefs.
// Panics on duplicate type names so misconfiguration is caught at construction.
func NewRegistry(types ...TypeDef) *Registry {
	m := make(map[string]TypeDef, len(types))
	for _, td := range types {
		if _, exists := m[td.Type]; exists {
			panic("artifact registry: duplicate type " + td.Type)
		}
		m[td.Type] = td
	}
	return &Registry{types: m}
}

// Unmarshal decodes a JSON array of artifacts using the registered types.
func (r *Registry) Unmarshal(data []byte) ([]Artifact, error) {
	var rawArtifacts []json.RawMessage
	if err := json.Unmarshal(data, &rawArtifacts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal artifacts array: %w", err)
	}
	artifacts := make([]Artifact, 0, len(rawArtifacts))
	for i, raw := range rawArtifacts {
		a, err := r.unmarshalOne(raw)
		if err != nil {
			return nil, fmt.Errorf("artifact[%d]: %w", i, err)
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, nil
}

// unmarshalOne decodes a single JSON artifact.
func (r *Registry) unmarshalOne(data []byte) (Artifact, error) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to determine artifact type: %w", err)
	}
	td, ok := r.types[env.Type]
	if !ok {
		return nil, fmt.Errorf("unknown artifact type: %q", env.Type)
	}
	a := td.New()
	if err := json.Unmarshal(data, a); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s artifact: %w", env.Type, err)
	}
	return a, nil
}

// Validate checks field-level constraints for the artifact at index i.
func (r *Registry) Validate(i int, a Artifact) error {
	field := fmt.Sprintf("artifacts[%d]", i)
	if a.ArtifactID() == "" {
		return apperrors.Validation(field+".id", fmt.Sprintf("artifact[%d]: id is required", i))
	}
	td, ok := r.types[a.ArtifactType()]
	if !ok {
		return fmt.Errorf("artifact[%d]: unknown type %q", i, a.ArtifactType())
	}
	if td.Validate == nil {
		return nil
	}
	return td.Validate(field, a)
}

// SourcePath returns the filesystem source path for an artifact, or "" if none.
func (r *Registry) SourcePath(a Artifact) string {
	td, ok := r.types[a.ArtifactType()]
	if !ok || td.SourcePath == nil {
		return ""
	}
	return td.SourcePath(a)
}

// TypedValidator wraps a type-safe validate function, hiding the type assertion.
func TypedValidator[T Artifact](fn func(field string, a T) error) func(field string, a Artifact) error {
	return func(field string, a Artifact) error {
		typed, ok := a.(T)
		if !ok {
			return fmt.Errorf("artifact type mismatch: expected %T, got %T", *new(T), a)
		}
		return fn(field, typed)
	}
}

// TypedSourcePath wraps a type-safe source path function, hiding the type assertion.
func TypedSourcePath[T Artifact](fn func(a T) string) func(a Artifact) string {
	return func(a Artifact) string {
		typed, ok := a.(T)
		if !ok {
			return ""
		}
		return fn(typed)
	}
}

var (
	defaultRegistryOnce sync.Once
	defaultReg          *Registry
)

// DefaultRegistry returns a Registry pre-loaded with all built-in artifact types.
// It is computed on first call and cached for subsequent calls.
func DefaultRegistry() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultReg = NewRegistry(
			DownloadDef,
			UploadDef,
			WriteDef,
			ReadDef,
			ArchiveDef,
			UnarchiveDef,
			MountDef,
			ListDef,
			StatDef,
		)
	})
	return defaultReg
}
