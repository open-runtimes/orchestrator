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

	pools, err := pool.LoadPools(config.GetEnv("POOLS_JSON", ""))
	if err != nil {
		slog.Error("Invalid pool configuration", "error", err)
		os.Exit(1)
	}
	orchestrator, err := buildOrchestrator(ctx, backend,
		config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest"),
		config.GetEnv("POOL_SHIM_IMAGE", "pool-shim:latest"), pools, metrics)
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

	if len(pools) > 0 {
		slog.Info("Revision pools configured", "count", len(pools))
	}

	// Data-plane listener (Docker): requests can legitimately run for minutes —
	// the per-request timeout lives in the workload-sidecar — so no
	// WriteTimeout here.
	var extra []*http.Server
	if deploymentsActivator != nil {
		extra = append(extra, &http.Server{
			Addr:              ":" + dataPort,
			Handler:           deploymentsActivator,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	healthChecker := health.NewChecker(orchestrator)
	router := api.NewDeploymentsRouter(api.DeploymentsRouterConfig{
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

func buildOrchestrator(ctx context.Context, backend, sidecarImage, poolShimImage string, pools []pool.Pool, metrics *observability.Metrics) (deployment.Orchestrator, error) {
	switch backend {
	case "docker":
		if len(pools) > 0 {
			return nil, errors.New("pools require the Kubernetes backend")
		}
		cfg := depdocker.LoadConfigFromEnv()
		cfg.SidecarImage = sidecarImage
		return depdocker.NewOrchestrator(ctx, cfg)
	case "kubernetes":
		cfg, err := depkubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, err
		}
		cfg.SidecarImage = sidecarImage
		cfg.PoolShimImage = poolShimImage
		cfg.Pools = pools
		cfg.Metrics = metrics
		return depkubernetes.NewOrchestrator(ctx, cfg)
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}
