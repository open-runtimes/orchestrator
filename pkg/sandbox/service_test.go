package sandbox

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/pool"
	"strings"
	"testing"
)

// fakeOrchestrator records what the service asked for and reports ready.
type fakeOrchestrator struct {
	last *Request
}

func (f *fakeOrchestrator) Start(context.Context) error { return nil }
func (f *fakeOrchestrator) Pools(context.Context) ([]pool.Status, error) {
	return []pool.Status{{ID: "py", Warm: 2}}, nil
}

func (f *fakeOrchestrator) Create(_ context.Context, req *Request) (*Status, error) {
	f.last = req
	return &Status{ID: req.ID, PoolID: req.Pool, State: StateReady, URL: "http://s-" + req.Token + ".example.test"}, nil
}

func (f *fakeOrchestrator) Status(context.Context, string) (*Status, error) { return &Status{}, nil }
func (f *fakeOrchestrator) List(context.Context) ([]Status, error)         { return nil, nil }
func (f *fakeOrchestrator) Delete(context.Context, string) error           { return nil }
func (f *fakeOrchestrator) Ready(context.Context) error                    { return nil }
func (f *fakeOrchestrator) Close() error                                   { return nil }

func testService(pools ...pool.Pool) (*Service, *fakeOrchestrator) {
	if len(pools) == 0 {
		pools = []pool.Pool{{ID: "py", Image: "img", Command: "/usr/local/bin/sandbox", Port: 3000, Size: 1}}
	}
	orch := &fakeOrchestrator{}
	return NewService(orch, nil, pools, artifact.DefaultRegistry()), orch
}

func TestCreate_MintsAnUnguessableTokenIndependentOfTheID(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	first, err := svc.Create(context.Background(), &Request{ID: "my-agent", Pool: "py"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	firstToken := orch.last.Token
	if len(firstToken) != 2*tokenBytes {
		t.Errorf("token: want %d hex chars (128 bits), got %d", 2*tokenBytes, len(firstToken))
	}
	if strings.Contains(first.URL, "my-agent") {
		t.Error("the caller-chosen id must never be the address — it is guessable")
	}

	// Same id, new sandbox, new capability: the token is not derived from the id.
	if _, err := svc.Create(context.Background(), &Request{ID: "my-agent", Pool: "py"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.Token == firstToken {
		t.Error("tokens must not repeat across sandboxes")
	}
}

func TestCreate_GeneratesIDWhenAbsent(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	if _, err := svc.Create(context.Background(), &Request{Pool: "py"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(orch.last.ID, "py-") {
		t.Errorf("generated id: got %q", orch.last.ID)
	}
}

func TestCreate_UnknownPoolNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := testService()

	_, err := svc.Create(context.Background(), &Request{Pool: "nope"})
	if !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreate_CommandFallsBackToThePool(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	if _, err := svc.Create(context.Background(), &Request{Pool: "py"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.Command != "" {
		t.Errorf("the service must leave the fallback to the backend, got %q", orch.last.Command)
	}

	// A pool with no command of its own and no request command is a 400, not a
	// sandbox that starts and immediately exits.
	bare, _ := testService(pool.Pool{ID: "py", Image: "img", Port: 3000})
	_, err := bare.Create(context.Background(), &Request{Pool: "py"})
	if !errors.Is(err, apperrors.ErrValidation) {
		t.Fatalf("want a validation error, got %v", err)
	}
}

func TestCreate_ValidatesID(t *testing.T) {
	t.Parallel()
	svc, _ := testService()

	for _, id := range []string{"Bad-Case", "under_score", "-leading", strings.Repeat("a", 64)} {
		if _, err := svc.Create(context.Background(), &Request{ID: id, Pool: "py"}); !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("id %q: want a validation error, got %v", id, err)
		}
	}
}

// A pool's idle ceiling is operator policy: an abandoned sandbox holds a warm
// pod hostage, so "until DELETE" is only honored where the pool allows it.
func TestCreate_AppliesThePoolIdleCeiling(t *testing.T) {
	t.Parallel()
	svc, orch := testService(pool.Pool{ID: "py", Image: "img", Command: "run", Port: 3000, MaxIdleSeconds: 900})

	if _, err := svc.Create(context.Background(), &Request{Pool: "py"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.IdleTimeoutSeconds != 900 {
		t.Errorf("omitted idle timeout must take the pool ceiling, got %d", orch.last.IdleTimeoutSeconds)
	}

	if _, err := svc.Create(context.Background(), &Request{Pool: "py", IdleTimeoutSeconds: 60}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.IdleTimeoutSeconds != 60 {
		t.Errorf("a shorter idle timeout must be honored, got %d", orch.last.IdleTimeoutSeconds)
	}

	_, err := svc.Create(context.Background(), &Request{Pool: "py", IdleTimeoutSeconds: 5000})
	if !errors.Is(err, apperrors.ErrValidation) {
		t.Fatalf("over the ceiling: want a validation error, got %v", err)
	}
}

func TestCreate_DefaultsTheRequestTimeout(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	if _, err := svc.Create(context.Background(), &Request{Pool: "py"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.TimeoutSeconds != defaultTimeout {
		t.Errorf("timeoutSeconds default: got %d", orch.last.TimeoutSeconds)
	}
	if _, err := svc.Create(context.Background(), &Request{Pool: "py", TimeoutSeconds: maxTimeoutSecs + 1}); !errors.Is(err, apperrors.ErrValidation) {
		t.Error("want a validation error over the timeout ceiling")
	}
}
