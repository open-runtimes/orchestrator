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

// NewRouter creates the management router for the jobs service.
func NewRouter(cfg RouterConfig) http.Handler {
	return NewOrchestratorRouter(OrchestratorRouterConfig{
		Metrics:         cfg.Metrics,
		HealthChecker:   cfg.HealthChecker,
		APIKey:          cfg.APIKey,
		JobService:      cfg.JobService,
		ArtifactEmitter: cfg.ArtifactEmitter,
	})
}

// registerJobRoutes mounts the jobs surface: the public /v1/jobs endpoints and
// the internal artifact endpoint, which carries per-job token auth of its own
// (the workload container shares the network path with the sidecar, so network
// isolation is not enough).
func registerJobRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler, cfg OrchestratorRouterConfig) {
	h := NewHandler(cfg.JobService, cfg.Metrics, cfg.ArtifactEmitter)

	artifactAuth := ArtifactAuthMiddleware(cfg.APIKey)
	mux.Handle("POST /internal/jobs/{jobId}/artifact", artifactAuth(http.HandlerFunc(h.ReportArtifact)))

	mux.Handle("POST /v1/jobs", auth(http.HandlerFunc(h.CreateJob)))
	mux.Handle("GET /v1/jobs", auth(http.HandlerFunc(h.ListJobs)))
	mux.Handle("GET /v1/jobs/{jobId}", auth(http.HandlerFunc(h.GetJob)))
	mux.Handle("DELETE /v1/jobs/{jobId}", auth(http.HandlerFunc(h.DeleteJob)))
}
