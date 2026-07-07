package artifact

import (
	"context"
	"orchestrator/internal/config"
)

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

// S3Configurable is implemented by artifacts that transfer over s3:// and need
// SigV4 credentials. The runner injects the service's credentials before Apply;
// artifacts that never touch S3 do not implement it.
type S3Configurable interface {
	SetS3Credentials(config.S3Credentials)
}

// Result represents the outcome of applying an artifact.
type Result struct {
	Status  string
	Content any // For event/list types - content to include in callback
	Error   error
}
