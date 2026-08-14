package api

import (
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/internal/sandbox"
)

// SandboxesRouterConfig holds dependencies for the sandboxes API router.
type SandboxesRouterConfig struct {
	Service       *sandbox.Service
	Metrics       *observability.Metrics
	HealthChecker *health.Checker
	APIKey        string
}

// NewSandboxesRouter creates the management router for the sandboxes service
// (the data plane is the sandbox proxy's own listener).
func NewSandboxesRouter(cfg SandboxesRouterConfig) http.Handler {
	return NewOrchestratorRouter(OrchestratorRouterConfig{
		Metrics:        cfg.Metrics,
		HealthChecker:  cfg.HealthChecker,
		APIKey:         cfg.APIKey,
		SandboxService: cfg.Service,
	})
}

// sandboxesHandler serves /v1/sandbox and /v1/sandbox-pool. Pools are
// config-defined, so the surface over them is read-only; sandboxes themselves
// are created and torn down by callers.
//
// Exec and files are deliberately absent: they are served by the sandbox image
// at the sandbox's own URL, which keeps this control plane off the data path.
type sandboxesHandler struct {
	svc *sandbox.Service
}

// registerSandboxRoutes mounts the sandbox routes.
//nolint:dupl // a route table, not logic: the shape it shares with the other planes is the point
func registerSandboxRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler, svc *sandbox.Service) {
	h := &sandboxesHandler{svc: svc}
	mux.Handle("POST /v1/sandbox", auth(http.HandlerFunc(h.create)))
	mux.Handle("GET /v1/sandbox", auth(http.HandlerFunc(h.list)))
	mux.Handle("GET /v1/sandbox/{id}", auth(http.HandlerFunc(h.get)))
	mux.Handle("DELETE /v1/sandbox/{id}", auth(http.HandlerFunc(h.remove)))
	mux.Handle("GET /v1/sandbox-pool", auth(http.HandlerFunc(h.pools)))
	mux.Handle("GET /v1/sandbox-pool/{id}", auth(http.HandlerFunc(h.pool)))
}

// create handles POST /v1/sandbox. Synchronous by design: a claim is
// sub-second, and the response carries the sandbox's URL — which is live when
// it is returned, since there is no per-sandbox gateway route to wait on.
func (h *sandboxesHandler) create(w http.ResponseWriter, r *http.Request) {
	req, ok := parseBody(w, r, parseSandbox)
	if !ok {
		return
	}
	status, err := h.svc.Create(r.Context(), req)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, status)
}

// parseSandbox decodes a create body, rejecting unknown fields — a typo'd
// field must fail loudly, not silently create a sandbox with defaults.
func parseSandbox(data []byte) (*sandbox.Request, error) {
	var req sandbox.Request
	if err := artifact.UnmarshalStrict(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func (h *sandboxesHandler) list(w http.ResponseWriter, r *http.Request) {
	sandboxes, err := h.svc.List(r.Context())
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sandbox.ListResponse{Sandboxes: sandboxes})
}

func (h *sandboxesHandler) get(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *sandboxesHandler) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), r.PathValue("id")); err != nil {
		handleServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *sandboxesHandler) pools(w http.ResponseWriter, r *http.Request) {
	pools, err := h.svc.Pools(r.Context())
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
}

func (h *sandboxesHandler) pool(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Pool(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
