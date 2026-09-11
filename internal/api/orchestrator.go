package api

import (
	"net/http"
	"orchestrator/internal/deployment"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
	"orchestrator/internal/sandbox"
)

// OrchestratorRouterConfig holds the dependencies of the combined router. Every
// service is optional: a nil one mounts no routes, which is how the
// single-plane services (jobs, deployments, sandbox) each get their own surface
// out of this one wiring, and how the all-in-one orchestrator binary gets all
// of them on a single listener.
type OrchestratorRouterConfig struct {
	Metrics       *observability.Metrics
	HealthChecker *health.Checker
	APIKey        string

	JobService   *job.Service
	JobCallbacks *job.CallbackEmitter // required with JobService: where artifact reports are emitted

	DeploymentService *deployment.Service
	SandboxService    *sandbox.Service
}

// NewOrchestratorRouter mounts every configured plane on one mux behind one
// middleware chain.
func NewOrchestratorRouter(cfg OrchestratorRouterConfig) http.Handler {
	mux := http.NewServeMux()
	registerHealthRoutes(mux, cfg.HealthChecker)

	auth := authMiddleware(cfg.APIKey)
	if cfg.JobService != nil {
		registerJobRoutes(mux, auth, cfg)
	}
	if cfg.DeploymentService != nil {
		registerDeploymentRoutes(mux, auth, cfg.DeploymentService)
	}
	if cfg.SandboxService != nil {
		registerSandboxRoutes(mux, auth, cfg.SandboxService)
	}

	return withMiddleware(mux, cfg.Metrics)
}

// withMiddleware applies the chain every router shares (order matters:
// outermost first). mux is passed to the metrics middleware so it can label
// series by route pattern rather than by raw path.
func withMiddleware(mux *http.ServeMux, metrics *observability.Metrics) http.Handler {
	var h http.Handler = mux
	h = jSONErrorMiddleware(h)
	h = contentTypeMiddleware(h)
	h = cORSMiddleware(h)
	h = metricsMiddleware(metrics, mux)(h)
	h = loggingMiddleware(h)
	h = recoveryMiddleware(h)
	return h
}
