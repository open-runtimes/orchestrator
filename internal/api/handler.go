// Package api provides the HTTP API handlers and routing for the jobs service.
package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
)

// maxRequestBodySize limits request body to 1MB to prevent memory exhaustion
const maxRequestBodySize = 1 << 20 // 1 MB

// parseBody reads a size-capped request body through one of the strict Parse
// functions (job.Parse, deployment.Parse, pool.Parse — the types whose custom
// UnmarshalJSON hides field names from DisallowUnknownFields). ok=false means
// the 400 is already written.
func parseBody[T any](w http.ResponseWriter, r *http.Request, parse func([]byte) (*T, error)) (*T, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err == nil {
		v, perr := parse(body)
		if perr == nil {
			return v, true
		}
		err = perr
	}
	writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
	return nil, false
}

// decodeStrict decodes a size-capped request body, rejecting unknown fields —
// a typo'd field name must fail loudly, not be silently dropped. Only for
// types without a custom UnmarshalJSON; those go through parseBody instead.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return artifact.UnmarshalStrict(body, v)
}

// ArtifactEmitter receives artifact results from the sidecar and dispatches
// the corresponding CloudEvents through the delivery pipeline.
type ArtifactEmitter interface {
	EmitArtifactEvent(report job.ArtifactReport)
}

// Handler contains HTTP handlers for the jobs API
type Handler struct {
	svc             *job.Service
	metrics         *observability.Metrics
	artifactEmitter ArtifactEmitter
}

// NewHandler creates a new API handler
func NewHandler(svc *job.Service, metrics *observability.Metrics, ae ArtifactEmitter) *Handler {
	return &Handler{
		svc:             svc,
		metrics:         metrics,
		artifactEmitter: ae,
	}
}

// CreateJob handles POST /v1/jobs
func (h *Handler) CreateJob(w http.ResponseWriter, r *http.Request) {
	req, ok := parseBody(w, r, job.Parse)
	if !ok {
		return
	}

	resp, err := h.svc.Create(r.Context(), req)
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

// writeJSON writes a JSON response
func (h *Handler) writeJSON(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, data)
}

// writeError writes an error response
func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	writeError(w, status, message)
}

// writeJSON writes a JSON response. Shared by the jobs and deployments handlers.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode response", "error", err)
	}
}

// writeError writes an error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// handleServiceError maps a service-layer error to its HTTP status.
func handleServiceError(w http.ResponseWriter, r *http.Request, err error) {
	status := apperrors.HTTPStatus(err)
	if status >= 500 {
		slog.Error("Internal error", "error", err, "path", r.URL.Path)
	} else {
		slog.Warn("Client error", "error", err, "path", r.URL.Path, "status", status)
	}
	writeError(w, status, err.Error())
}

// handleError handles errors from service layer with appropriate HTTP status codes.
func (h *Handler) handleError(w http.ResponseWriter, r *http.Request, err error) {
	handleServiceError(w, r, err)
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

	// Deliberately lenient (no unknown-field rejection): the sender is the
	// job sidecar, which may be a release ahead of or behind this service
	// during a rolling upgrade.
	var report job.ArtifactReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid artifact report: "+err.Error())
		return
	}
	report.JobID = jobID
	if h.metrics != nil {
		h.metrics.RecordArtifactTask(r.Context(), report.Type, report.Format, report.Compression,
			report.Status == "success", report.DurationSeconds, report.OutputBytes)
	}

	if h.artifactEmitter != nil {
		h.artifactEmitter.EmitArtifactEvent(report)
	}

	w.WriteHeader(http.StatusAccepted)
}
