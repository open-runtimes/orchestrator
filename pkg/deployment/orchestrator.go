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
	Lifecycle

	// Apply creates the deployment or replaces its spec in place. Applying an
	// identical spec is a no-op. Reports whether the deployment was created —
	// the backend knows for free, and the API's 201-vs-200 hangs on it.
	Apply(ctx context.Context, req *Request) (created bool, err error)

	// Delete tears down the deployment's workloads and routing state.
	Delete(ctx context.Context, id string) error

	// Scale sets the deployment's replica count. 0 scales to zero (idle);
	// the workload's materialized state is retained for a fast cold start.
	// Docker clamps any positive count to 1.
	Scale(ctx context.Context, id string, replicas int) error

	// Spec returns the last-applied request, reconstructed from the backend.
	Spec(ctx context.Context, id string) (*Request, error)

	Routing

	// Status returns the deployment's current state, derived from the backend.
	Status(ctx context.Context, id string) (*StatusResponse, error)

	// List returns all deployments' statuses.
	List(ctx context.Context) ([]StatusResponse, error)
}

// Routing is the traffic surface of an orchestrator backend.
type Routing interface {
	// SetTraffic replaces the deployment's traffic table — canary,
	// blue-green, or rollback are all weight edits across existing
	// revisions. Also switches the rollout mode to manual: a new revision
	// no longer auto-cuts until traffic is reset to a single 100% target on
	// the latest revision. Empty targets release back to auto: the backend
	// resolves "latest" and routes 100% to it. Docker is single-revision:
	// empty targets or 100% to the deployment's own ID are the no-op it
	// already does, anything else is a validation error.
	SetTraffic(ctx context.Context, id string, targets []Target) error

	// Endpoints returns ready proxy endpoints for the deployment's
	// traffic-receiving revisions — the in-process activator forwards
	// requests to these (Docker data path).
	Endpoints(ctx context.Context, id string) ([]*url.URL, error)
}

// Lifecycle is the process-lifecycle surface of an orchestrator backend.
type Lifecycle interface {
	// Start reconciles pre-existing deployments and begins maintenance.
	Start(ctx context.Context) error

	// Ready checks that the backend is reachable.
	Ready(ctx context.Context) error

	// Close releases orchestrator resources. Running deployments are NOT
	// stopped — they continue serving independently.
	Close() error
}
