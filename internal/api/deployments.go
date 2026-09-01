package api

import (
	"net/http"
	"orchestrator/internal/deployment"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
)

// DeploymentsRouterConfig holds dependencies for the deployments API router.
type DeploymentsRouterConfig struct {
	Service       *deployment.Service
	Metrics       *observability.Metrics
	HealthChecker *health.Checker
	APIKey        string
}

// NewDeploymentsRouter creates the management router for the deployments
// service (the data plane is the activator's own listener).
func NewDeploymentsRouter(cfg DeploymentsRouterConfig) http.Handler {
	return NewOrchestratorRouter(OrchestratorRouterConfig{
		Metrics:           cfg.Metrics,
		HealthChecker:     cfg.HealthChecker,
		APIKey:            cfg.APIKey,
		DeploymentService: cfg.Service,
	})
}

// registerDeploymentRoutes mounts the deployments surface.
//
//nolint:dupl // a route table, not logic: the shape it shares with the other planes is the point
func registerDeploymentRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler, svc *deployment.Service) {
	h := &deploymentsHandler{svc: svc}
	mux.Handle("POST /v1/deployments", auth(http.HandlerFunc(h.apply)))
	mux.Handle("GET /v1/deployments", auth(http.HandlerFunc(h.list)))
	mux.Handle("GET /v1/deployments/{id}", auth(http.HandlerFunc(h.get)))
	mux.Handle("GET /v1/deployments/{id}/revisions", auth(http.HandlerFunc(h.revisions)))
	mux.Handle("POST /v1/deployments/{id}/traffic", auth(http.HandlerFunc(h.setTraffic)))
	mux.Handle("DELETE /v1/deployments/{id}", auth(http.HandlerFunc(h.remove)))
}

type deploymentsHandler struct {
	svc *deployment.Service
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
