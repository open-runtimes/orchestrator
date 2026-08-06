// deployments-service is the serving plane: long-lived HTTP workloads
// (/v1/deployments) with an in-process activator data plane. See docs/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/activator"
	"orchestrator/internal/api"
	"orchestrator/internal/artifact"
	"orchestrator/internal/autoscaler"
	"orchestrator/internal/config"
	"orchestrator/internal/deployment"
	depdocker "orchestrator/internal/deployment/docker"
	depkubernetes "orchestrator/internal/deployment/kubernetes"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/internal/pool"
	poolkubernetes "orchestrator/internal/pool/kubernetes"
	"orchestrator/internal/sandbox"
	sandboxdocker "orchestrator/internal/sandbox/docker"
	sandboxkubernetes "orchestrator/internal/sandbox/kubernetes"
	"orchestrator/internal/server"
	"orchestrator/internal/workload"
	"os"
	"time"
)

func main() {
	ctx := context.Background()
	svcCfg := config.LoadServiceConfig()
	backend := config.GetEnv("ORCHESTRATOR_BACKEND", "docker")
	domain := config.GetEnv("DEPLOYMENTS_DOMAIN", "localhost")
	dataPort := config.GetEnv("DATA_PORT", "8081")

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "deployments", "backend", backend))

	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		slog.Error("Failed to init metrics", "error", err)
		os.Exit(1)
	}

	orchestrator, err := buildOrchestrator(ctx, backend, config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest"), metrics)
	if err != nil {
		slog.Error("Failed to build orchestrator", "error", err)
		os.Exit(1)
	}
	defer orchestrator.Close()

	if err := orchestrator.Start(ctx); err != nil {
		slog.Error("Failed to start orchestrator", "error", err)
		os.Exit(1)
	}
	slog.Info("Orchestrator ready")

	// Data plane and URL shape differ by backend: on Kubernetes the gateway
	// is the data plane (HTTPRoute per deployment, host on port 80) and the
	// idle signal comes from scraping sidecar /stats; on Docker the
	// in-process activator serves data on dataPort and, being on-path for
	// all traffic, is itself the activity source.
	urlFor := func(host string) string {
		if backend == "kubernetes" || dataPort == "80" {
			return "http://" + host
		}
		return "http://" + host + ":" + dataPort
	}
	svc := deployment.NewService(orchestrator, metrics, artifact.MountingRegistry(), domain, urlFor)

	eventDispatcher := dispatcher.NewMemory(dispatcher.LoadConfigFromEnv(), metrics)

	// The autoscaler's metric sources differ by backend: sidecar /stats
	// scrapes supply warm concurrency on both; the cold hold-up signal comes
	// from the standalone activator's /stats on Kubernetes and directly from
	// the in-process activator on Docker.
	concurrency := autoscaler.NewSidecarConcurrency(orchestrator, workload.DefaultAdminPort)
	var queue autoscaler.QueueSource
	var deploymentsActivator *activator.Activator
	if backend == "kubernetes" {
		queue = autoscaler.NewActivatorQueue(config.GetEnv("ACTIVATOR_STATS_URL", "http://deployments-activator:8081/stats"))
	} else {
		deploymentsActivator = activator.New(svc, eventDispatcher, metrics)
		queue = autoscaler.QueuedDepthFunc(deploymentsActivator.QueuedDepth)
	}

	scaler := autoscaler.New(orchestrator, concurrency, queue, autoscaler.LoadConfigFromEnv(), metrics)
	scalerCtx, stopScaler := context.WithCancel(ctx)
	defer stopScaler()
	// Exactly one replica may write scales: gate under the backend's lease
	// when it offers one (the K8s orchestrator; Docker runs directly).
	if gated, ok := orchestrator.(interface {
		RunLeaderElected(ctx context.Context, run func(context.Context))
	}); ok {
		go gated.RunLeaderElected(scalerCtx, scaler.Run)
	} else {
		go scaler.Run(scalerCtx)
	}

	// Pools are config-declared: no POOLS_JSON → no pool orchestrator, no
	// pool routes.
	pools, err := pool.LoadPools(config.GetEnv("POOLS_JSON", ""))
	if err != nil {
		slog.Error("Invalid pool configuration", "error", err)
		os.Exit(1)
	}
	var poolSvc *pool.Service
	if len(pools) > 0 {
		poolOrchestrator, err := buildPoolOrchestrator(ctx, backend, pools, metrics)
		if err != nil {
			slog.Error("Failed to build pool orchestrator", "error", err)
			os.Exit(1)
		}
		defer poolOrchestrator.Close()
		if err := poolOrchestrator.Start(ctx); err != nil {
			slog.Error("Failed to start pool orchestrator", "error", err)
			os.Exit(1)
		}
		poolSvc = pool.NewService(poolOrchestrator, metrics, pools, artifact.MountingRegistry())
		slog.Info("Pools configured", "count", len(pools))
	}

	// Sandbox pools are config-declared too: no SANDBOX_POOLS_JSON → no
	// sandbox orchestrator, no sandbox routes.
	sandboxPools, err := sandbox.LoadPools(config.GetEnv("SANDBOX_POOLS_JSON", ""))
	if err != nil {
		slog.Error("Invalid sandbox pool configuration", "error", err)
		os.Exit(1)
	}
	var sandboxSvc *sandbox.Service
	var sandboxProxy *activator.SandboxProxy
	if len(sandboxPools) > 0 {
		sandboxOrchestrator, err := buildSandboxOrchestrator(ctx, backend, sandboxPools, metrics)
		if err != nil {
			slog.Error("Failed to build sandbox orchestrator", "error", err)
			os.Exit(1)
		}
		defer sandboxOrchestrator.Close()
		if err := sandboxOrchestrator.Start(ctx); err != nil {
			slog.Error("Failed to start sandbox orchestrator", "error", err)
			os.Exit(1)
		}
		sandboxSvc = sandbox.NewService(sandboxOrchestrator, metrics, sandboxPools, artifact.MountingRegistry())
		slog.Info("Sandbox pools configured", "count", len(sandboxPools))

		// On Docker the sandbox data plane runs in-process, resolving tokens
		// straight from the daemon; on Kubernetes it is its own Deployment behind
		// the wildcard route (cmd/sandbox-proxy).
		if targets, ok := sandboxOrchestrator.(activator.SandboxTargets); ok {
			sandboxProxy = activator.NewSandboxProxy(targets, activator.SandboxConfig{
				Domain: config.GetEnv("SANDBOX_DOMAIN", "localhost"),
				Hold:   time.Duration(config.GetIntEnv("SANDBOX_HOLD_SECONDS", 5)) * time.Second,
			}, metrics)
		}
	}

	// Data-plane listener (Docker): requests can legitimately run for minutes —
	// the per-request timeout lives in the workload-sidecar — so no
	// WriteTimeout here.
	var extra []*http.Server
	if deploymentsActivator != nil {
		extra = append(extra, &http.Server{
			Addr:              ":" + dataPort,
			Handler:           dataHandler(deploymentsActivator, sandboxProxy),
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	healthChecker := health.NewChecker(orchestrator)
	router := api.NewDeploymentsRouter(api.DeploymentsRouterConfig{
		Service:        svc,
		Metrics:        metrics,
		HealthChecker:  healthChecker,
		APIKey:         svcCfg.APIKey,
		PoolService:    poolSvc,
		Dispatcher:     eventDispatcher,
		SandboxService: sandboxSvc,
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
		Cleanup: func(cleanupCtx context.Context) {
			slog.Info("Draining callback dispatcher")
			if err := eventDispatcher.Close(cleanupCtx); err != nil {
				slog.Warn("Dispatcher shutdown error", "error", err)
			}
			slog.Info("Running deployments will continue independently")
		},
	}); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

func buildPoolOrchestrator(ctx context.Context, backend string, pools []pool.Pool, metrics *observability.Metrics) (pool.Orchestrator, error) {
	sidecarImage := config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest")
	shimImage := config.GetEnv("POOL_SHIM_IMAGE", "pool-shim:latest")
	switch backend {
	case "docker":
		return nil, errors.New("pools require the Kubernetes backend")
	case "kubernetes":
		cfg, err := poolkubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, err
		}
		cfg.SidecarImage = sidecarImage
		cfg.ShimImage = shimImage
		cfg.Pools = pools
		cfg.Metrics = metrics
		return poolkubernetes.NewOrchestrator(ctx, cfg)
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}

// buildSandboxOrchestrator builds the sandbox backend. The Docker one is for
// development: no warm pool (creates are cold) and no isolation tiers, since
// gvisor and kata are RuntimeClasses. See docs/sandboxes.md.
// dataHandler picks which activator serves a request. Both data planes share the
// one Docker listener, so the Host decides, and pkg/sandbox owns that decision:
// a sandbox host is s-{token}.{sandbox domain}, and the "s-" prefix is required,
// so every other host under the domain falls through to deployments. Give
// sandboxes their own domain if a deployment host could itself start with "s-".
func dataHandler(deployments http.Handler, sandboxes *activator.SandboxProxy) http.Handler {
	if sandboxes == nil {
		return deployments
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sandboxes.Matches(r.Host) {
			sandboxes.ServeHTTP(w, r)
			return
		}
		deployments.ServeHTTP(w, r)
	})
}

func buildSandboxOrchestrator(ctx context.Context, backend string, pools []pool.Pool, metrics *observability.Metrics) (sandbox.Orchestrator, error) {
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

func buildOrchestrator(ctx context.Context, backend, sidecarImage string, metrics *observability.Metrics) (deployment.Orchestrator, error) {
	switch backend {
	case "docker":
		cfg := depdocker.LoadConfigFromEnv()
		cfg.SidecarImage = sidecarImage
		return depdocker.NewOrchestrator(ctx, cfg)
	case "kubernetes":
		cfg, err := depkubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, err
		}
		cfg.SidecarImage = sidecarImage
		cfg.Metrics = metrics
		return depkubernetes.NewOrchestrator(ctx, cfg)
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}
