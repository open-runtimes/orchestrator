// deployments-service is the HTTP API server for the serving plane:
// long-lived HTTP workloads (/v1/deployments) and pre-warmed pools
// (/v1/deployment-pools). See docs/design.
//
// Phase 0 skeleton: config, backend selection, middleware, probes, and
// metrics only — the deployment orchestrator arrives in Phase 1.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"orchestrator/internal/api"
	"orchestrator/internal/config"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/pkg/server"
	"os"
)

func main() {
	ctx := context.Background()
	svcCfg := config.LoadServiceConfig()
	backend := config.GetEnv("ORCHESTRATOR_BACKEND", "docker")

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "deployments", "backend", backend))

	if backend != "docker" && backend != "kubernetes" {
		slog.Error("Unknown orchestrator backend (expected docker|kubernetes)", "backend", backend)
		os.Exit(1)
	}

	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		slog.Error("Failed to init metrics", "error", err)
		os.Exit(1)
	}

	// Phase 1 wires the deployment orchestrator's Ready here; until then the
	// service is ready as soon as it can serve.
	checker := health.NewChecker(alwaysReady{})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, checker.Liveness(r.Context()))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		resp := checker.Readiness(r.Context())
		status := http.StatusOK
		if !resp.IsHealthy() {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, resp)
	})

	var handler http.Handler = mux
	if metrics != nil {
		handler = api.MetricsMiddleware(metrics)(handler)
	}
	handler = api.LoggingMiddleware()(handler)
	handler = api.RecoveryMiddleware()(handler)

	if err := server.Serve(ctx, server.Options{
		Handler:        handler,
		MetricsHandler: metricsHandler,
		Port:           svcCfg.Port,
		MetricsPort:    svcCfg.MetricsPort,
		DrainWait:      svcCfg.ShutdownDrainWait,
		SetDraining:    checker.SetShuttingDown,
	}); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

// alwaysReady satisfies health.ReadinessChecker until Phase 1 supplies the
// deployment orchestrator.
type alwaysReady struct{}

func (alwaysReady) Ready(context.Context) error { return nil }

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode response", "error", err)
	}
}
