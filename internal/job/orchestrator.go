// Package job defines the Orchestrator interface and job-related types.
package job

import "context"

// Orchestrator defines the interface for container orchestration platforms.
// Implementations handle the full job lifecycle including container management,
// log streaming, and callback delivery.
//
// # State Management
//
// The Orchestrator is the SOURCE OF TRUTH for job state. Job state is stored in
// the orchestration layer (e.g., Docker labels, K8s annotations) rather than in
// the jobs-service process. This enables:
//
//   - Crash recovery: Running jobs continue if jobs-service restarts
//   - Horizontal scaling: Multiple instances can coexist
//   - Simplicity: No external database required
//
// # Callbacks
//
// The Orchestrator is responsible for sending callbacks:
//   - job.start - when job container starts
//   - job.log - streamed from container stdout/stderr
//   - job.exit - when job container exits
//
// Input/output callbacks are handled by the sidecar.
type Orchestrator interface {
	// Run creates and starts a job with its sidecar.
	// The job runs asynchronously; use Status to check progress.
	// Returns an error if a job with the same ID already exists.
	Run(ctx context.Context, req *Request) error

	// Stop stops a running job and cleans up its resources.
	// This is idempotent - stopping an already-stopped job returns nil.
	Stop(ctx context.Context, jobID string) error

	// Status returns the current status of a job.
	// Returns an error if the job does not exist.
	Status(ctx context.Context, jobID string) (*Status, error)

	// List returns the status of all jobs.
	List(ctx context.Context) ([]Status, error)

	// Ready checks if the orchestrator backend is reachable.
	// For Docker: verifies the daemon is reachable.
	// For K8s: verifies the API server is reachable.
	Ready(ctx context.Context) error

	// Close releases resources held by the orchestrator.
	// Running jobs are NOT stopped - they continue independently.
	Close() error
}
