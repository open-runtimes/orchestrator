package api

import (
	"encoding/json"
	"net/http"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/pkg/deployment"
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
	h := &deploymentsHandler{svc: cfg.Service, health: cfg.HealthChecker}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", h.livez)
	mux.HandleFunc("GET /readyz", h.readyz)

	auth := AuthMiddleware(cfg.APIKey)
	mux.Handle("POST /v1/deployments", auth(http.HandlerFunc(h.apply)))
	mux.Handle("GET /v1/deployments", auth(http.HandlerFunc(h.list)))
	mux.Handle("GET /v1/deployments/{id}", auth(http.HandlerFunc(h.get)))
	mux.Handle("DELETE /v1/deployments/{id}", auth(http.HandlerFunc(h.remove)))

	var handler http.Handler = mux
	handler = ContentTypeMiddleware()(handler)
	handler = CORSMiddleware()(handler)
	if cfg.Metrics != nil {
		handler = MetricsMiddleware(cfg.Metrics)(handler)
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
func (h *deploymentsHandler) apply(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var req deployment.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	status, err := h.svc.Apply(r.Context(), &req)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
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
