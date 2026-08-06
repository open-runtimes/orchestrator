package api

import (
	"net/http"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
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

	// Internal endpoints - per-job token auth (the workload container shares
	// the network path with the sidecar, so network isolation is not enough)
	artifactAuth := ArtifactAuthMiddleware(cfg.APIKey)
	mux.Handle("POST /internal/jobs/{jobId}/artifact", artifactAuth(http.HandlerFunc(handler.ReportArtifact)))

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
		h = MetricsMiddleware(cfg.Metrics, mux)(h)
	}
	h = LoggingMiddleware()(h)
	h = RecoveryMiddleware()(h)

	return h
}
