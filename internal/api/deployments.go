package api

import (
	"net/http"
	"orchestrator/internal/deployment"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/internal/pool"
	"orchestrator/internal/sandbox"
)

// DeploymentsRouterConfig holds dependencies for the deployments API router.
type DeploymentsRouterConfig struct {
	Service       *deployment.Service
	Metrics       *observability.Metrics
	HealthChecker *health.Checker
	APIKey        string

	// PoolService mounts /v1/deployment-pools when pools are configured
	// (nil = no pool routes). Dispatcher delivers async activation results.
	PoolService *pool.Service
	Dispatcher  dispatcher.Queue

	// SandboxService mounts /v1/sandbox and /v1/sandbox-pool when sandbox
	// pools are configured (nil = no sandbox routes).
	SandboxService *sandbox.Service
}

// NewDeploymentsRouter creates the management router for the deployments
// service (the data plane is the activator's own listener).
func NewDeploymentsRouter(cfg DeploymentsRouterConfig) http.Handler {
	h := &deploymentsHandler{svc: cfg.Service, health: cfg.HealthChecker}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", h.livez)
	mux.HandleFunc("GET /readyz", h.readyz)

	auth := AuthMiddleware(cfg.APIKey)
	mux.Handle("POST /v1/deployments", auth(http.HandlerFunc(h.apply)))
	mux.Handle("GET /v1/deployments", auth(http.HandlerFunc(h.list)))
	mux.Handle("GET /v1/deployments/{id}", auth(http.HandlerFunc(h.get)))
	mux.Handle("GET /v1/deployments/{id}/revisions", auth(http.HandlerFunc(h.revisions)))
	mux.Handle("POST /v1/deployments/{id}/traffic", auth(http.HandlerFunc(h.setTraffic)))
	mux.Handle("DELETE /v1/deployments/{id}", auth(http.HandlerFunc(h.remove)))

	if cfg.PoolService != nil {
		registerPoolRoutes(mux, auth, cfg.PoolService, cfg.Dispatcher)
	}
	if cfg.SandboxService != nil {
		registerSandboxRoutes(mux, auth, cfg.SandboxService)
	}

	var handler http.Handler = mux
	handler = JSONErrorMiddleware()(handler)
	handler = ContentTypeMiddleware()(handler)
	handler = CORSMiddleware()(handler)
	if cfg.Metrics != nil {
		handler = MetricsMiddleware(cfg.Metrics, mux)(handler)
	}
	handler = LoggingMiddleware()(handler)
	handler = RecoveryMiddleware()(handler)
	return handler
}

type deploymentsHandler struct {
	svc    *deployment.Service
	health *health.Checker
}

// apply handles POST /v1/deployments — declarative create-or-update.
// 201 when the deployment is new, 200 when an existing one is updated.
func (h *deploymentsHandler) apply(w http.ResponseWriter, r *http.Request) {
	req, ok := parseBody(w, r, deployment.Parse)
	if !ok {
		return
	}

	status, created, err := h.svc.Apply(r.Context(), req)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, status)
}

func (h *deploymentsHandler) get(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *deploymentsHandler) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// setTraffic handles POST /v1/deployments/{id}/traffic — canary, blue-green,
// or rollback as weight edits across existing revisions. An empty target
// list releases traffic back to auto mode (100% on the latest revision,
// auto-cut resumed).
func (h *deploymentsHandler) setTraffic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Targets []deployment.Target `json:"targets"`
	}
	if err := decodeStrict(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	status, err := h.svc.SetTraffic(r.Context(), r.PathValue("id"), req.Targets)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// revisions handles GET /v1/deployments/{id}/revisions.
func (h *deploymentsHandler) revisions(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": status.Revisions, "traffic": status.Traffic})
}

func (h *deploymentsHandler) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), r.PathValue("id")); err != nil {
		handleServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *deploymentsHandler) livez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.health.Liveness(r.Context()))
}

func (h *deploymentsHandler) readyz(w http.ResponseWriter, r *http.Request) {
	response := h.health.Readiness(r.Context())
	status := http.StatusOK
	if !response.IsHealthy() {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, response)
}
