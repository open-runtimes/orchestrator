package api

import (
	"net/http"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
)

// RouterConfig holds dependencies for the router.
type RouterConfig struct {
	JobService    *job.Service
	Metrics       *observability.Metrics
	HealthChecker *health.Checker
	Dispatcher    dispatcher.Dispatcher
	APIKey        string
}

// NewRouter creates a new HTTP router with all routes configured.
func NewRouter(cfg RouterConfig) http.Handler {
	handler := NewHandler(cfg.JobService, cfg.Metrics, cfg.HealthChecker, cfg.Dispatcher)

	mux := http.NewServeMux()

	// Health check endpoints (liveness/readiness probes) - no auth required
	mux.HandleFunc("GET /livez", handler.Livez)
	mux.HandleFunc("GET /readyz", handler.Readyz)

	// Internal endpoints - no auth (network-isolated)
	mux.HandleFunc("POST /internal/events", handler.ProxyEvent)

	// Job endpoints - auth required
	authMiddleware := AuthMiddleware(cfg.APIKey)
	mux.Handle("POST /v1/jobs", authMiddleware(http.HandlerFunc(handler.CreateJob)))
	mux.Handle("GET /v1/jobs", authMiddleware(http.HandlerFunc(handler.ListJobs)))
	mux.Handle("GET /v1/jobs/{jobId}", authMiddleware(http.HandlerFunc(handler.GetJob)))
	mux.Handle("DELETE /v1/jobs/{jobId}", authMiddleware(http.HandlerFunc(handler.DeleteJob)))

	// Apply middleware chain (order matters: outermost first)
	var h http.Handler = mux
	h = ContentTypeMiddleware()(h)
	h = CORSMiddleware()(h)
	if cfg.Metrics != nil {
		h = MetricsMiddleware(cfg.Metrics)(h)
	}
	h = LoggingMiddleware()(h)
	h = RecoveryMiddleware()(h)

	return h
}
