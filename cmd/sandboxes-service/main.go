// sandboxes-service is the sandbox control plane: live, isolated workspaces
// created over /v1/sandbox and driven over HTTP at their own hostnames. Pools
// (SANDBOX_POOLS_JSON) are optional warm capacity — a create may name an image
// instead. See docs/sandboxes.md.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/activator"
	"orchestrator/internal/api"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/internal/pool"
	"orchestrator/internal/sandbox"
	sandboxdocker "orchestrator/internal/sandbox/docker"
	sandboxkubernetes "orchestrator/internal/sandbox/kubernetes"
	"orchestrator/internal/server"
	"os"
	"time"
)

func main() {
	ctx := context.Background()
	svcCfg := config.LoadServiceConfig()
	backend := config.GetEnv("ORCHESTRATOR_BACKEND", "docker")

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "sandboxes", "backend", backend))

	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		slog.Error("Failed to init metrics", "error", err)
		os.Exit(1)
	}

	pools, err := sandbox.LoadPools(config.GetEnv("SANDBOX_POOLS_JSON", ""))
	if err != nil {
		slog.Error("Invalid sandbox pool configuration", "error", err)
		os.Exit(1)
	}

	orchestrator, err := buildOrchestrator(ctx, backend, pools, metrics)
	if err != nil {
		slog.Error("Failed to build orchestrator", "error", err)
		os.Exit(1)
	}
	defer orchestrator.Close()

	if err := orchestrator.Start(ctx); err != nil {
		slog.Error("Failed to start orchestrator", "error", err)
		os.Exit(1)
	}
	slog.Info("Orchestrator ready", "pools", len(pools))

	svc := sandbox.NewService(orchestrator, metrics, pools, artifact.MountingRegistry())

	// Data plane: on Kubernetes it is its own Deployment behind the wildcard
	// route (cmd/sandbox-proxy); on Docker it runs in-process, resolving
	// tokens straight from the daemon, on its own listener.
	var extra []*http.Server
	if targets, ok := orchestrator.(activator.SandboxTargets); ok {
		proxy := activator.NewSandboxProxy(targets, activator.SandboxConfig{
			Domain: config.GetEnv("SANDBOX_DOMAIN", "localhost"),
			Hold:   time.Duration(config.GetIntEnv("SANDBOX_HOLD_SECONDS", 5)) * time.Second,
		}, metrics)
		extra = append(extra, &http.Server{
			Addr:              ":" + config.GetEnv("DATA_PORT", "8081"),
			Handler:           proxy,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	healthChecker := health.NewChecker(orchestrator)
	router := api.NewSandboxesRouter(api.SandboxesRouterConfig{
		Service:       svc,
		Metrics:       metrics,
		HealthChecker: healthChecker,
		APIKey:        svcCfg.APIKey,
	})

	if svcCfg.APIKey == "" {
		slog.Warn("API authentication disabled - no API_KEY configured")
	}

	if err := server.Serve(ctx, server.Options{
		Handler:        router,
		MetricsHandler: metricsHandler,
		Port:           svcCfg.Port,
		MetricsPort:    svcCfg.MetricsPort,
		Extra:          extra,
		DrainWait:      svcCfg.ShutdownDrainWait,
		SetDraining:    healthChecker.SetShuttingDown,
	}); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

// buildOrchestrator builds the sandbox backend. The Docker one is for
// development: no warm pool (creates are cold) and no isolation tiers, since
// gvisor and kata are RuntimeClasses. See docs/sandboxes.md.
func buildOrchestrator(ctx context.Context, backend string, pools []pool.Pool, metrics *observability.Metrics) (sandbox.Orchestrator, error) {
	switch backend {
	case "docker":
		cfg := sandboxdocker.LoadConfigFromEnv()
		cfg.SidecarImage = config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest")
		cfg.Pools = pools
		return sandboxdocker.NewOrchestrator(ctx, cfg)
	case "kubernetes":
		cfg, err := sandboxkubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, err
		}
		cfg.SidecarImage = config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest")
		cfg.ShimImage = config.GetEnv("POOL_SHIM_IMAGE", "pool-shim:latest")
		cfg.Pools = pools
		cfg.Metrics = metrics
		return sandboxkubernetes.NewOrchestrator(ctx, cfg)
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}
