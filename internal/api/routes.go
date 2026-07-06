package api

import (
	"net/http"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/pkg/job"
)

// RouterConfig holds dependencies for the router.
type RouterConfig struct {
	JobService      *job.Service
	Metrics         *observability.Metrics
	HealthChecker   *health.Checker
	ArtifactEmitter ArtifactEmitter
	APIKey          string
}

// NewRouter creates a new HTTP router with all routes configured.
func NewRouter(cfg RouterConfig) http.Handler {
	handler := NewHandler(cfg.JobService, cfg.Metrics, cfg.HealthChecker, cfg.ArtifactEmitter)

	mux := http.NewServeMux()

	// Health check endpoints (liveness/readiness probes) - no auth required
	mux.HandleFunc("GET /livez", handler.Livez)
	mux.HandleFunc("GET /readyz", handler.Readyz)

	// Internal endpoints - no auth (network-isolated)
	mux.HandleFunc("POST /internal/jobs/{jobId}/artifact", handler.ReportArtifact)

	// Job endpoints - auth required
	authMiddleware := AuthMiddleware(cfg.APIKey)
	mux.Handle("POST /v1/jobs", authMiddleware(http.HandlerFunc(handler.CreateJob)))
	mux.Handle("GET /v1/jobs", authMiddleware(http.HandlerFunc(handler.ListJobs)))
	mux.Handle("GET /v1/jobs/{jobId}", authMiddleware(http.HandlerFunc(handler.GetJob)))
	mux.Handle("DELETE /v1/jobs/{jobId}", authMiddleware(http.HandlerFunc(handler.DeleteJob)))

	// Apply middleware chain (order matters: outermost first)
	var h http.Handler = mux
	h = JSONErrorMiddleware()(h)
	h = ContentTypeMiddleware()(h)
	h = CORSMiddleware()(h)
	if cfg.Metrics != nil {
		h = MetricsMiddleware(cfg.Metrics)(h)
	}
	h = LoggingMiddleware()(h)
	h = RecoveryMiddleware()(h)

	return h
}
