package artifact

import "context"

// The post-phase sentinels. An artifact that depends on one of these runs AFTER
// the workload rather than before it — upload the results, archive the
// workspace, snapshot it somewhere durable.
//
// WorkloadDependency is the spelling to use: the rule is about the workload,
// whatever kind it is, and "job" reads wrong on a sandbox. JobDependency is the
// original and stays accepted forever — it is on the wire in callers' specs.
const (
	WorkloadDependency = "workload"
	JobDependency      = "job"
)

// PostPhase reports whether a dependency value names the post phase.
func PostPhase(depends string) bool {
	return depends == WorkloadDependency || depends == JobDependency
}

// which is what makes a teardown something a caller waits on rather than a
// deletion.
func HasPostPhase(artifacts []Artifact) bool {
	_, post := Partition(artifacts)
	return len(post) > 0
}

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

	// Format and Compression describe what the artifact actually turned out
	// to be. Consumers sniff them from the header; archive producers report
	// the selected encoder profile directly, preserving lz4hc versus lz4.
	// They are two axes on purpose: "squashfs" and "gzip" answer different
	// questions, and the filename cannot reliably supply either one.
	Format      string // "tar", "squashfs", or "erofs"
	Compression string // "none", "gzip", "zstd", "lz4", "lz4hc", "xz", "lzma", "lzo"
}
