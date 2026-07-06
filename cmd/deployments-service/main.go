// deployments-service is the serving plane: long-lived HTTP workloads
// (/v1/deployments) with an in-process activator data plane. See docs/design.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/activator"
	"orchestrator/internal/api"
	"orchestrator/internal/artifact"
	"orchestrator/internal/autoscaler"
	"orchestrator/internal/config"
	depdocker "orchestrator/internal/deployment/docker"
	depkubernetes "orchestrator/internal/deployment/kubernetes"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	pooldocker "orchestrator/internal/pool/docker"
	poolkubernetes "orchestrator/internal/pool/kubernetes"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"orchestrator/pkg/pool"
	"orchestrator/pkg/server"
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

	orchestrator, err := buildOrchestrator(ctx, backend, config.GetEnv("DEPLOYMENT_SIDECAR_IMAGE", "deployments-sidecar:latest"))
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
	svc := deployment.NewService(orchestrator, artifact.DefaultRegistry(), domain, urlFor)

	eventDispatcher := dispatcher.NewMemory(dispatcher.LoadConfigFromEnv(), metrics)

	// The autoscaler's metric sources differ by backend: sidecar /stats
	// scrapes supply warm concurrency on both; the cold hold-up signal comes
	// from the standalone activator's /stats on Kubernetes and directly from
	// the in-process activator on Docker.
	concurrency := autoscaler.NewSidecarConcurrency(orchestrator, proxy.DefaultAdminPort)
	var queue autoscaler.QueueSource
	var extra []*http.Server
	if backend == "kubernetes" {
		queue = autoscaler.NewActivatorQueue(config.GetEnv("ACTIVATOR_STATS_URL", "http://deployments-activator:8081/stats"))
	} else {
		act := activator.New(svc, eventDispatcher)
		queue = autoscaler.QueuedDepthFunc(act.QueuedDepth)
		// Data-plane listener: requests can legitimately run for minutes
		// (the per-request timeout lives in the deployments-sidecar), so no
		// WriteTimeout here.
		extra = append(extra, &http.Server{
			Addr:              ":" + dataPort,
			Handler:           act,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	scaler := autoscaler.New(orchestrator, concurrency, queue, autoscaler.LoadConfigFromEnv())
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
		poolOrchestrator, err := buildPoolOrchestrator(ctx, backend, pools)
		if err != nil {
			slog.Error("Failed to build pool orchestrator", "error", err)
			os.Exit(1)
		}
		defer poolOrchestrator.Close()
		if err := poolOrchestrator.Start(ctx); err != nil {
			slog.Error("Failed to start pool orchestrator", "error", err)
			os.Exit(1)
		}
		poolSvc = pool.NewService(poolOrchestrator, pools, artifact.DefaultRegistry())
		slog.Info("Pools configured", "count", len(pools))
	}

	healthChecker := health.NewChecker(orchestrator)
	router := api.NewDeploymentsRouter(api.DeploymentsRouterConfig{
		Service:       svc,
		Metrics:       metrics,
		HealthChecker: healthChecker,
		APIKey:        svcCfg.APIKey,
		PoolService:   poolSvc,
		Dispatcher:    eventDispatcher,
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

func buildPoolOrchestrator(ctx context.Context, backend string, pools []pool.Pool) (pool.Orchestrator, error) {
	sidecarImage := config.GetEnv("DEPLOYMENT_SIDECAR_IMAGE", "deployments-sidecar:latest")
	shimImage := config.GetEnv("POOL_SHIM_IMAGE", "pool-shim:latest")
	switch backend {
	case "docker":
		cfg := pooldocker.LoadConfigFromEnv()
		cfg.SidecarImage = sidecarImage
		cfg.ShimImage = shimImage
		cfg.Pools = pools
		return pooldocker.NewOrchestrator(ctx, cfg)
	case "kubernetes":
		cfg, err := poolkubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, err
		}
		cfg.SidecarImage = sidecarImage
		cfg.ShimImage = shimImage
		cfg.Pools = pools
		return poolkubernetes.NewOrchestrator(ctx, cfg)
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}

func buildOrchestrator(ctx context.Context, backend, sidecarImage string) (deployment.Orchestrator, error) {
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
		return depkubernetes.NewOrchestrator(ctx, cfg)
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}
