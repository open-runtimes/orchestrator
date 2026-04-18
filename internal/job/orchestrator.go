// Package job defines the Orchestrator interface and job-related types.
package job

import (
	"context"
	"errors"
)

// Orchestrator defines the interface for container orchestration platforms.
// Implementations handle the full job lifecycle including container management,
// log streaming, and callback delivery.
//
// # State Management
//
// Job state is managed through a shared Store with FSM-enforced transitions.
// The Store is populated by the orchestration backend during reconciliation
// and updated as jobs progress through their lifecycle.
//
// # Events
//
// The Orchestrator emits lifecycle events through a CallbackEmitter:
//   - job.start - when job container starts
//   - job.log - streamed from container stdout/stderr
//   - job.exit - when job container exits
//
// Both Store and CallbackEmitter are provided via OrchestratorFactory, enforcing
// that all implementations receive these shared dependencies.
// Register listeners on the CallbackEmitter before calling NewOrchestrator.
// Input/output callbacks are handled by the sidecar.
type Orchestrator interface {
	// Start initializes the orchestrator, reconciling any pre-existing jobs
	// and beginning background maintenance.
	Start(ctx context.Context) error

	// Run creates and starts a job with its sidecar.
	// The job runs asynchronously; use Status to check progress.
	// Returns an error if a job with the same ID already exists.
	Run(ctx context.Context, req *Request) error

	// Stop stops a running job and cleans up its resources.
	// This is idempotent - stopping an already-stopped job returns nil.
	Stop(ctx context.Context, jobID string) error

	// Status returns the current status of a job.
	// Returns an error if the job does not exist.
	Status(ctx context.Context, jobID string) (*StatusResponse, error)

	// List returns the status of all jobs.
	List(ctx context.Context) ([]StatusResponse, error)

	// Ready checks if the orchestrator backend is reachable.
	// For Docker: verifies the daemon is reachable.
	// For K8s: verifies the API server is reachable.
	Ready(ctx context.Context) error

	// Close releases resources held by the orchestrator.
	// Running jobs are NOT stopped - they continue independently.
	Close() error
}

// OrchestratorFactory builds an Orchestrator with required shared dependencies.
// Implementations return this from their constructor so that the CallbackEmitter
// is guaranteed to be provided.
type OrchestratorFactory func(emitter *CallbackEmitter) (Orchestrator, error)

// NewOrchestrator validates that shared dependencies are non-nil and calls the factory.
func NewOrchestrator(emitter *CallbackEmitter, factory OrchestratorFactory) (Orchestrator, error) {
	if emitter == nil {
		return nil, errors.New("emitter is required")
	}
	return factory(emitter)
}
