package pool

import "context"

// Orchestrator materializes warm pools and their activations. The backend is
// the source of truth: a claimed pod (labeled with its activation), its
// Service/route, and the sidecar's own claim state are all reconstructable on
// Start — the service holds nothing.
type Orchestrator interface {
	// Start reconciles existing warm/claimed pods and begins the
	// leader-elected replenishment loops.
	Start(ctx context.Context) error

	// Pools reports the configured pools with live warm/claimed counts.
	Pools(ctx context.Context) ([]Status, error)

	// Activate claims a warm pod and late-binds the activation onto it,
	// returning once the workload is serving, with its URL. No free warm pod
	// → the pool's burst policy decides (cold create, or a 429-mapped error).
	Activate(ctx context.Context, poolID string, act *Activation) (*ActivationStatus, error)

	// Status returns one activation's state, derived from the backend.
	Status(ctx context.Context, poolID, activationID string) (*ActivationStatus, error)

	// List returns the pool's live activations.
	List(ctx context.Context, poolID string) ([]ActivationStatus, error)

	// Deactivate tears the activation down; its pod is discarded (never
	// reused) and the slot replenished off the request path.
	Deactivate(ctx context.Context, poolID, activationID string) error

	// Ready checks that the backend is reachable.
	Ready(ctx context.Context) error

	// Close releases orchestrator resources.
	Close() error
}
