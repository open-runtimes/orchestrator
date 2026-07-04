package deployment

import (
	"context"
	"net/url"
)

// Orchestrator materializes deployments as running, routable workloads.
// Mirrors pkg/job.Orchestrator with Run→Apply (declarative create-or-update)
// and Endpoints for the activator. Phase 1 is single-revision: Apply replaces
// in place; immutable revisions, SetTraffic, and Retire arrive in Phase 3.
//
// The backend is the source of truth — Status, List, Spec, and Endpoints
// derive from it live, so any replica can serve any request and a restart
// loses nothing.
type Orchestrator interface {
	// Start reconciles pre-existing deployments and begins maintenance.
	Start(ctx context.Context) error

	// Apply creates the deployment or replaces its spec in place. Applying an
	// identical spec is a no-op.
	Apply(ctx context.Context, req *Request) error

	// Delete tears down the deployment's workloads and routing state.
	Delete(ctx context.Context, id string) error

	// Spec returns the last-applied request, reconstructed from the backend.
	Spec(ctx context.Context, id string) (*Request, error)

	// Endpoints returns ready proxy endpoints for the deployment — the
	// activator forwards requests to these.
	Endpoints(ctx context.Context, id string) ([]*url.URL, error)

	// Status returns the deployment's current state, derived from the backend.
	Status(ctx context.Context, id string) (*StatusResponse, error)

	// List returns all deployments' statuses.
	List(ctx context.Context) ([]StatusResponse, error)

	// Ready checks that the backend is reachable.
	Ready(ctx context.Context) error

	// Close releases orchestrator resources. Running deployments are NOT
	// stopped — they continue serving independently.
	Close() error
}
