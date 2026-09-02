package sandbox

import (
	"context"
	"errors"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/pool"
	"orchestrator/internal/volume"
	"strings"
	"testing"
)

// fakeOrchestrator records what the service asked for and reports ready.
type fakeOrchestrator struct {
	last *Request
}

func (f *fakeOrchestrator) Start(context.Context) error { return nil }
func (f *fakeOrchestrator) Create(_ context.Context, req *Request) (*Status, error) {
	f.last = req
	return &Status{ID: req.ID, PoolID: req.Pool, State: StateReady, URL: "http://s-" + req.Token + ".example.test"}, nil
}

func (f *fakeOrchestrator) Status(_ context.Context, id string) (*Status, error) {
	return &Status{ID: id, State: StateReady}, nil
}
func (f *fakeOrchestrator) List(context.Context) ([]Status, error) { return nil, nil }
func (f *fakeOrchestrator) Delete(context.Context, string) error   { return nil }
func (f *fakeOrchestrator) Ready(context.Context) error            { return nil }
func (f *fakeOrchestrator) Close() error                           { return nil }

func testService(pools ...pool.Pool) (*Service, *fakeOrchestrator) {
	if len(pools) == 0 {
		pools = []pool.Pool{{ID: "py", Size: 1, Spec: pool.Spec{Image: "img", Port: 3000, CPU: 1, Memory: 512}}}
	}
	orch := &fakeOrchestrator{}
	return NewService(orch, nil, pools, artifact.MountingRegistry()), orch
}

func standardRequest() *Request { return &Request{Image: "img", Port: 3000, CPU: 1, Memory: 512} }

func TestLoadPools_RequiresUnambiguousFixedShapes(t *testing.T) {
	t.Parallel()
	valid := `[{"id":"py","image":"img","port":3000,"cpu":1,"memory":512}]`
	if _, err := LoadPools(valid); err != nil {
		t.Fatalf("valid transparent pool: %v", err)
	}
	for name, raw := range map[string]string{
		"missing resources": `[{"id":"py","image":"img","port":3000}]`,
		"command default":   `[{"id":"py","image":"img","port":3000,"cpu":1,"memory":512,"command":"run"}]`,
		"idle policy":       `[{"id":"py","image":"img","port":3000,"cpu":1,"memory":512,"maxIdleSeconds":900}]`,
		"duplicate shape":   `[{"id":"a","image":"img","port":3000,"cpu":1,"memory":512},{"id":"b","image":"img","port":3000,"cpu":1,"memory":512}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPools(raw); err == nil {
				t.Fatal("want invalid transparent pool config")
			}
		})
	}
}

func TestCreate_AutomaticallyMatchesTheCompleteShape(t *testing.T) {
	t.Parallel()
	svc, orch := testService()
	req := standardRequest()
	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.Pool != "py" || orch.last.Pool != "py" {
		t.Fatalf("matched pool: request=%q backend=%q", req.Pool, orch.last.Pool)
	}
}

func TestCreate_MintsAnUnguessableTokenIndependentOfTheID(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	req := standardRequest()
	req.ID = "my-agent"
	first, err := svc.Create(context.Background(), req)
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
	req = standardRequest()
	req.ID = "my-agent"
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.Token == firstToken {
		t.Error("tokens must not repeat across sandboxes")
	}
}

func TestCreate_GeneratesIDWhenAbsent(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	if _, err := svc.Create(context.Background(), standardRequest()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(orch.last.ID, "sbx-") {
		t.Errorf("generated id: got %q", orch.last.ID)
	}
}

func TestCreate_NoMatchingPoolStillCreatesDirectly(t *testing.T) {
	t.Parallel()
	svc, _ := testService()

	req := &Request{Image: "different", Port: 8080}
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch := req.Pool; orch != "" {
		t.Fatalf("unmatched request selected pool %q", orch)
	}
}

func TestCreate_LeavesTheCommandToTheBackend(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	if _, err := svc.Create(context.Background(), standardRequest()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.Command != "" {
		t.Errorf("the service must leave the fallback to the backend, got %q", orch.last.Command)
	}

	// A pool that names no command is the ordinary case, not an error: its image
	// is just a runtime, and the backend runs the agent it installed into the
	// workspace.
	bare, _ := testService(pool.Pool{ID: "py", Spec: pool.Spec{Image: "node:22-slim", Port: 3000}})
	if _, err := bare.Create(context.Background(), &Request{Image: "node:22-slim", Port: 3000}); err != nil {
		t.Fatalf("a matching pool without a command must be accepted: %v", err)
	}
}

func TestCreate_ValidatesID(t *testing.T) {
	t.Parallel()
	svc, _ := testService()

	for _, id := range []string{"Bad-Case", "under_score", "-leading", strings.Repeat("a", 64)} {
		req := standardRequest()
		req.ID = id
		if _, err := svc.Create(context.Background(), req); !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("id %q: want a validation error, got %v", id, err)
		}
	}
}

// Pool selection cannot alter request semantics: idle expiry belongs to the
// sandbox request whether acquisition is warm or direct.
func TestCreate_PreservesTheRequestedIdleTimeout(t *testing.T) {
	t.Parallel()
	svc, orch := testService(pool.Pool{ID: "py", MaxIdleSeconds: 900, Spec: pool.Spec{Image: "img", Port: 3000}})

	if _, err := svc.Create(context.Background(), standardRequest()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.IdleTimeoutSeconds != 0 {
		t.Errorf("omitted idle timeout changed to %d", orch.last.IdleTimeoutSeconds)
	}

	req := standardRequest()
	req.IdleTimeoutSeconds = 60
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.IdleTimeoutSeconds != 60 {
		t.Errorf("a shorter idle timeout must be honored, got %d", orch.last.IdleTimeoutSeconds)
	}

	req = standardRequest()
	req.IdleTimeoutSeconds = 5000
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("pool policy must not reject a valid request: %v", err)
	}
}

func TestCreate_DefaultsTheRequestTimeout(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	if _, err := svc.Create(context.Background(), standardRequest()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.TimeoutSeconds == nil || *orch.last.TimeoutSeconds != defaultTimeout {
		t.Errorf("omitted timeoutSeconds must take the default: got %v", orch.last.TimeoutSeconds)
	}
	req := standardRequest()
	req.TimeoutSeconds = ptrTo(maxTimeoutSecs + 1)
	if _, err := svc.Create(context.Background(), req); !errors.Is(err, apperrors.ErrValidation) {
		t.Error("want a validation error over the timeout ceiling")
	}
	req = standardRequest()
	req.TimeoutSeconds = ptrTo(-1)
	if _, err := svc.Create(context.Background(), req); !errors.Is(err, apperrors.ErrValidation) {
		t.Error("want a validation error for a negative timeout")
	}

	// An explicit 0 is the documented escape hatch for long-lived sessions
	// (WebSocket terminals, language servers). It must survive validation, or
	// the connection it was asked for is cut at the default five minutes.
	req = standardRequest()
	req.TimeoutSeconds = ptrTo(0)
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.last.TimeoutSeconds == nil || *orch.last.TimeoutSeconds != 0 {
		t.Errorf("explicit 0 must reach the backend unchanged: got %v", orch.last.TimeoutSeconds)
	}
}

// Mounting changes the pod, and warm pods are built before any claim — so the
// pool decides, and a request against a pool without the capability is refused
// before a pod is claimed for it.
func TestCreate_MountNeedsThePoolCapability(t *testing.T) {
	t.Parallel()
	mount := func() artifact.Set {
		return artifact.Set{&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"}}
	}

	plain, _ := testService()
	plainReq := &Request{ID: "sbx", Image: "img", Port: 3000, Artifacts: mount()}
	if _, err := plain.Create(t.Context(), plainReq); err != nil {
		t.Fatalf("a non-matching mount request must create directly: %v", err)
	}
	if plainReq.Pool != "" {
		t.Errorf("plain pool must not match a mount-capable shape")
	}

	// Declared: accepted, and the backend gets the mount to perform.
	capable, orch := testService(pool.Pool{ID: "sqfs", Size: 1, Spec: pool.Spec{Image: "img", Port: 3000, Mounts: true}})
	capableReq := &Request{ID: "sbx", Image: "img", Port: 3000, Artifacts: mount()}
	if _, err := capable.Create(t.Context(), capableReq); err != nil {
		t.Fatalf("a pool that declares mounts should accept one: %v", err)
	}
	if len(orch.last.Artifacts) != 1 {
		t.Errorf("the mount must reach the backend, got %v", orch.last.Artifacts)
	}
	if capableReq.Pool != "sqfs" {
		t.Errorf("mount-capable shape selected pool %q", capableReq.Pool)
	}
}

func TestCreate_ValidatesPorts(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	req := standardRequest()
	req.Ports = []int{5173, 9229}
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(orch.last.Ports) != 2 {
		t.Errorf("ports not passed through: %v", orch.last.Ports)
	}

	for name, ports := range map[string][]int{
		"sidecar data port":  {8000},
		"sidecar admin port": {8001},
		"the pool's own":     {3000},
		"out of range":       {70000},
		"zero":               {0},
		"duplicate":          {5173, 5173},
	} {
		req := standardRequest()
		req.Ports = ports
		if _, err := svc.Create(context.Background(), req); !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("%s (%v): want a validation error, got %v", name, ports, err)
		}
	}
}

// Every sandbox describes its complete pod shape. Pool configuration is never
// a request source and therefore cannot make an otherwise valid request fail.
func TestCreate_RequiresACompleteShape(t *testing.T) {
	t.Parallel()
	svc, orch := testService()

	tests := []struct {
		name string
		req  *Request
		want string
	}{
		{"no image", &Request{ID: "a"}, "image is required"},
		{"image without a port", &Request{ID: "a", Image: "img"}, "port is required"},
		{"port out of range", &Request{ID: "a", Image: "img", Port: 70000}, "must be 1-65535"},
		{"negative cpu", &Request{ID: "a", Image: "img", Port: 3000, CPU: -1}, "cpu must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Create(t.Context(), tt.req)
			if err == nil {
				t.Fatal("want a rejection")
			}
			if got := apperrors.HTTPStatus(err); got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%v)", got, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should say %q, got %q", tt.want, err)
			}
		})
	}

	// The poolless happy path: accepted, and the backend is handed the spec.
	status, err := svc.Create(t.Context(), &Request{
		ID: "solo", Image: "python:3.12-slim", Port: 3000, RuntimeClass: "gvisor",
	})
	if err != nil {
		t.Fatalf("poolless create: %v", err)
	}
	if orch.last.Image != "python:3.12-slim" || orch.last.Port != 3000 {
		t.Errorf("the backend must get the request's own shape, got %+v", orch.last)
	}
	if status.PoolID != "" {
		t.Errorf("there was no pool: got poolId %q", status.PoolID)
	}
}

// A poolless sandbox's shape comes straight off its request — no pool of one
// standing in between, so there is no size or burst policy to get wrong.
func TestShape_IsTheRequestsOwnPod(t *testing.T) {
	t.Parallel()
	shape := (&Request{ID: "solo", Image: "img", Port: 3000, CPU: 0.5, Memory: 256,
		Volumes: []volume.Volume{{Source: "pvc", Path: "/data"}}}).Shape()

	if shape.Image != "img" || shape.Port != 3000 || shape.CPU != 0.5 || shape.Memory != 256 {
		t.Errorf("the shape must carry the request's pod fields, got %+v", shape)
	}
	if len(shape.Volumes) != 1 {
		t.Errorf("a poolless sandbox attaches its own storage, got %+v", shape.Volumes)
	}
	if shape.Mounts {
		t.Error("no mount artifact, so no privilege")
	}

	// Mounting is inferred per request here, as it is for a job or a revision:
	// the pod is built for this request, so nothing had to be declared ahead.
	mounting := (&Request{ID: "solo", Image: "img", Port: 3000, Artifacts: artifact.Set{
		&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"},
	}}).Shape()
	if !mounting.Mounts {
		t.Error("a poolless sandbox that mounts must get the capability")
	}
}

// Fixed-shape differences simply bypass the pool; they are never ignored and
// never rejected because an operator happened to configure warm capacity.
func TestCreate_FixedShapeDifferencesBypassThePool(t *testing.T) {
	t.Parallel()
	svc, _ := testService()

	for _, tc := range []struct {
		name string
		req  *Request
	}{
		{"volumes", &Request{Image: "img", Port: 3000, Volumes: []volume.Volume{{Source: "pvc", Path: "/data"}}}},
		{"cpu", &Request{Image: "img", Port: 3000, CPU: 2}},
		{"runtimeClass", &Request{Image: "img", Port: 3000, RuntimeClass: "kata"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Create(context.Background(), tc.req); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tc.req.Pool != "" {
				t.Errorf("different shape selected pool %q", tc.req.Pool)
			}
		})
	}
}
