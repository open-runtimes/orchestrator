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
	// NeedsPostPhase marks a type whose work is NOT Apply's to do: it has to be
	// established before the worker starts and undone after it exits, which only
	// the post-phase sidecar can arrange. A workload that runs no post phase
	// cannot honour such a type at all, so a registry built for one rejects it
	// (see ServingRegistry) instead of accepting it and quietly doing nothing.
	NeedsPostPhase bool
}

// Registry holds a set of artifact type definitions. It is immutable after
// construction and safe for concurrent use without locking.
type Registry struct {
	types map[string]TypeDef
	// postPhase reports whether the consumer runs the post-phase sidecar, and so
	// whether the types that depend on one can be honoured here.
	postPhase bool
}

// NewRegistry builds a Registry for a consumer that runs the whole artifact
// lifecycle, post phase included.
// Panics on duplicate type names so misconfiguration is caught at construction.
func NewRegistry(types ...TypeDef) *Registry {
	return buildRegistry(true, types)
}

func buildRegistry(postPhase bool, types []TypeDef) *Registry {
	m := make(map[string]TypeDef, len(types))
	for _, td := range types {
		if _, exists := m[td.Type]; exists {
			panic("artifact registry: duplicate type " + td.Type)
		}
		m[td.Type] = td
	}
	return &Registry{types: m, postPhase: postPhase}
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
	if td.NeedsPostPhase && !r.postPhase {
		return apperrors.Validation(field+".type", fmt.Sprintf(
			"artifact type %q cannot run here: it must be established before the workload starts and undone after it, and this workload has nothing to do that",
			a.ArtifactType()))
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

// builtinTypes lists every artifact type the service understands. Both
// registries below are built from this one list, so a new type is available
// everywhere it can work and rejected — never dropped — where it cannot.
func builtinTypes() []TypeDef {
	return []TypeDef{
		DownloadDef,
		UploadDef,
		WriteDef,
		ReadDef,
		ArchiveDef,
		UnarchiveDef,
		MountDef,
		ListDef,
		StatDef,
	}
}

var (
	defaultRegistryOnce  sync.Once
	defaultReg           *Registry
	servingRegistryOnce  sync.Once
	servingReg           *Registry
	mountingRegistryOnce sync.Once
	mountingReg          *Registry
)

// DefaultRegistry returns the Registry for jobs: every built-in type, including
// the ones needing the post-phase sidecar a job runs.
// It is computed on first call and cached for subsequent calls.
func DefaultRegistry() *Registry {
	defaultRegistryOnce.Do(func() { defaultReg = buildRegistry(true, builtinTypes()) })
	return defaultReg
}

// ServingRegistry returns the Registry for serving workloads that cannot mount:
// deployments and pool activations. They materialize artifacts in a pre phase
// only, with nothing to establish a mount or undo it, so a type needing that is
// a validation error here rather than a silent no-op.
func ServingRegistry() *Registry {
	servingRegistryOnce.Do(func() { servingReg = buildRegistry(false, builtinTypes()) })
	return servingReg
}

// MountingRegistry returns the Registry for sandboxes, whose resident sidecar
// CAN establish a mount and release it on shutdown — but only in a pod whose
// pool declared the capability. That is per-pool and this is per-type, so the
// type is permitted here and the pool gate lives in the sandbox service.
func MountingRegistry() *Registry {
	mountingRegistryOnce.Do(func() { mountingReg = buildRegistry(true, builtinTypes()) })
	return mountingReg
}
