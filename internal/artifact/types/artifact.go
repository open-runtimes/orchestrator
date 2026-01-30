package types

import "context"

// JobDependency is the special dependency value indicating an artifact runs after the job.
const JobDependency = "job"

// Artifact is the interface for all artifact types.
// Artifacts without "job" in their dependency chain run before the job (inputs).
// Artifacts that depend on "job" (directly or transitively) run after the job (outputs).
type Artifact interface {
	ArtifactID() string
	ArtifactType() string
	DependsOn() string
	Apply(ctx context.Context, basePath string) *Result
}

// Result represents the outcome of applying an artifact.
type Result struct {
	Status  string
	Content any // For event/list types - content to include in callback
	Error   error
}
