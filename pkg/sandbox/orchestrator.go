package sandbox

import (
	"context"
	"orchestrator/pkg/pool"
)

// Orchestrator materializes sandboxes on a backend. The backend is the source
// of truth: a claimed pod carries its sandbox id and capability token as
// labels and its spec as an annotation, so Status, List, and a service restart
// reconstruct everything by listing pods — the service holds nothing.
type Orchestrator interface {
	// Start reconciles existing warm/claimed pods and begins the
	// leader-elected replenishment and reaping loops.
	Start(ctx context.Context) error

	// Pools reports the configured sandbox pools with live warm/claimed counts.
	Pools(ctx context.Context) ([]pool.Status, error)

	// Create claims a warm pod, materializes the request's artifacts on it, and
	// returns once the sandbox's contract is being served at its URL. No free
	// warm pod → the pool's burst policy decides (cold create, or a 429-mapped
	// error).
	Create(ctx context.Context, req *Request) (*Status, error)

	// Status returns one sandbox's state, derived from the backend.
	Status(ctx context.Context, id string) (*Status, error)

	// List returns every live sandbox.
	List(ctx context.Context) ([]Status, error)

	// Delete tears the sandbox down, invalidating its URL. Its pod is
	// discarded (never reused) and the slot replenished off the request path.
	Delete(ctx context.Context, id string) error

	// Ready checks that the backend is reachable.
	Ready(ctx context.Context) error

	// Close releases orchestrator resources.
	Close() error
}
