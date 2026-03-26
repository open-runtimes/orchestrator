package artifact

import (
	"context"
	"log/slog"
)

// Partition splits artifacts into pre-job and post-job phases.
// An artifact is post-job if it depends on JobDependency directly or transitively.
// Order within each slice is preserved from the input.
func Partition(artifacts []Artifact) (preJob, postJob []Artifact) {
	dependsOnJob := make(map[string]bool)

	for _, a := range artifacts {
		if a.DependsOn() == JobDependency {
			dependsOnJob[a.ArtifactID()] = true
		}
	}

	changed := true
	for changed {
		changed = false
		for _, a := range artifacts {
			if !dependsOnJob[a.ArtifactID()] && dependsOnJob[a.DependsOn()] {
				dependsOnJob[a.ArtifactID()] = true
				changed = true
			}
		}
	}

	for _, a := range artifacts {
		if dependsOnJob[a.ArtifactID()] {
			postJob = append(postJob, a)
		} else {
			preJob = append(preJob, a)
		}
	}
	return preJob, postJob
}

// ApplyFunc is the per-artifact callback called by RunInOrder.
// The callback is responsible for calling a.Apply and any caller-specific I/O.
// A non-nil error is recorded but does not stop execution — remaining artifacts
// whose dependencies are satisfied will still run.
type ApplyFunc func(ctx context.Context, a Artifact) error

// RunInOrder executes artifacts in dependency order, calling fn for each one
// whose dependencies have been satisfied. The JobDependency sentinel is treated
// as always-satisfied. Artifacts with unresolvable dependencies (e.g. cycles or
// missing deps) are skipped with a warning log.
// Returns the last non-nil error from fn, or nil.
func RunInOrder(ctx context.Context, artifacts []Artifact, fn ApplyFunc) error {
	completed := make(map[string]bool)
	var lastErr error

	for len(completed) < len(artifacts) {
		progress := false
		for _, a := range artifacts {
			id := a.ArtifactID()
			if completed[id] {
				continue
			}
			dep := a.DependsOn()
			if dep == JobDependency {
				dep = ""
			}
			if dep != "" && !completed[dep] {
				continue
			}
			if err := fn(ctx, a); err != nil {
				lastErr = err
			}
			completed[id] = true
			progress = true
		}
		if !progress {
			for _, a := range artifacts {
				if !completed[a.ArtifactID()] {
					slog.Warn("artifact skipped due to unresolved dependency",
						"artifactId", a.ArtifactID(),
						"depends", a.DependsOn())
				}
			}
			break
		}
	}
	return lastErr
}
