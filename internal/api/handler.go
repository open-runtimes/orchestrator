// Package api provides the HTTP API handlers and routing for the jobs service.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/health"
	"orchestrator/pkg/job"
	"orchestrator/internal/observability"
)

// maxRequestBodySize limits request body to 1MB to prevent memory exhaustion
const maxRequestBodySize = 1 << 20 // 1 MB

// ArtifactEmitter receives artifact results from the sidecar and dispatches
// the corresponding CloudEvents through the delivery pipeline.
type ArtifactEmitter interface {
	EmitArtifactEvent(report job.ArtifactReport)
}

// Handler contains HTTP handlers for the jobs API
type Handler struct {
	svc             *job.Service
	metrics         *observability.Metrics
	health          *health.Checker
	artifactEmitter ArtifactEmitter
}

// NewHandler creates a new API handler
func NewHandler(svc *job.Service, metrics *observability.Metrics, healthChecker *health.Checker, ae ArtifactEmitter) *Handler {
	return &Handler{
		svc:             svc,
		metrics:         metrics,
		health:          healthChecker,
		artifactEmitter: ae,
	}
}

// CreateJob handles POST /v1/jobs
func (h *Handler) CreateJob(w http.ResponseWriter, r *http.Request) {
	// Limit request body size to prevent memory exhaustion
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var req job.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	resp, err := h.svc.Create(r.Context(), &req)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	h.writeJSON(w, http.StatusAccepted, resp)
}

// ListJobs handles GET /v1/jobs
func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	resp, err := h.svc.List(r.Context())
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	h.writeJSON(w, http.StatusOK, resp)
}

// GetJob handles GET /v1/jobs/{jobId}
func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	if jobID == "" {
		h.writeError(w, http.StatusBadRequest, "Job ID is required")
		return
	}

	status, err := h.svc.Get(r.Context(), jobID)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	h.writeJSON(w, http.StatusOK, status)
}

// DeleteJob handles DELETE /v1/jobs/{jobId}
func (h *Handler) DeleteJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	if jobID == "" {
		h.writeError(w, http.StatusBadRequest, "Job ID is required")
		return
	}

	if err := h.svc.Cancel(r.Context(), jobID); err != nil {
		h.handleError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Livez handles GET /livez - liveness probe.
// Returns 200 if the process is alive. Does not check dependencies.
func (h *Handler) Livez(w http.ResponseWriter, r *http.Request) {
	response := h.health.Liveness(r.Context())
	h.writeJSON(w, http.StatusOK, response)
}

// Readyz handles GET /readyz - readiness probe.
// Returns 200 if the service is ready to accept traffic.
// Returns 503 if dependencies (Docker) are unavailable.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	response := h.health.Readiness(r.Context())

	status := http.StatusOK
	if !response.IsHealthy() {
		status = http.StatusServiceUnavailable
	}

	h.writeJSON(w, status, response)
}

// writeJSON writes a JSON response
func (h *Handler) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode response", "error", err)
	}
}

// writeError writes an error response
func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, map[string]string{"error": message})
}

// handleError handles errors from service layer with appropriate HTTP status codes.
func (h *Handler) handleError(w http.ResponseWriter, r *http.Request, err error) {
	status := apperrors.HTTPStatus(err)
	if status >= 500 {
		slog.Error("Internal error", "error", err, "path", r.URL.Path)
	} else {
		slog.Warn("Client error", "error", err, "path", r.URL.Path, "status", status)
	}
	h.writeError(w, status, err.Error())
}

// ReportArtifact handles POST /internal/jobs/{jobId}/artifact.
// Called by the sidecar to report the result of an artifact operation.
// The orchestrator constructs the CloudEvent and dispatches it via the delivery pipeline.
func (h *Handler) ReportArtifact(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	if jobID == "" {
		h.writeError(w, http.StatusBadRequest, "job ID is required")
		return
	}

	var report job.ArtifactReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid artifact report: "+err.Error())
		return
	}
	report.JobID = jobID

	if h.artifactEmitter != nil {
		h.artifactEmitter.EmitArtifactEvent(report)
	}

	w.WriteHeader(http.StatusAccepted)
}
