package artifact

import "sync"

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

var (
	globalRegistryMu sync.RWMutex
	globalRegistry   = map[string]TypeDef{}
)

// Register adds a TypeDef to the global registry.
// Panics on duplicate type names so misconfiguration is caught at startup.
func Register(td TypeDef) {
	globalRegistryMu.Lock()
	defer globalRegistryMu.Unlock()
	if _, exists := globalRegistry[td.Type]; exists {
		panic("artifact registry: duplicate type " + td.Type)
	}
	globalRegistry[td.Type] = td
}

// SourcePath returns the filesystem source path for an artifact, or "" if none.
func SourcePath(a Artifact) string {
	globalRegistryMu.RLock()
	td, ok := globalRegistry[a.ArtifactType()]
	globalRegistryMu.RUnlock()
	if !ok || td.SourcePath == nil {
		return ""
	}
	return td.SourcePath(a)
}

// TypedValidator wraps a type-safe validate function, hiding the type assertion.
func TypedValidator[T Artifact](fn func(field string, a T) error) func(field string, a Artifact) error {
	return func(field string, a Artifact) error {
		return fn(field, a.(T))
	}
}

// TypedSourcePath wraps a type-safe source path function, hiding the type assertion.
func TypedSourcePath[T Artifact](fn func(a T) string) func(a Artifact) string {
	return func(a Artifact) string {
		return fn(a.(T))
	}
}
