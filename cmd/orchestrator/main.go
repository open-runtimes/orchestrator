// orchestrator is every control plane in one process: jobs, deployments (with
// their activator) and sandboxes (with their proxy), on one API port and one
// data port. It exists for `docker compose up` and local
// development — production runs the per-service images from the Helm chart,
// where each plane scales, fails and gets RBAC on its own.
//
// Docker backend only, for the same reason: on Kubernetes the planes are
// separate Deployments with separate leases, and a single process pretending
// otherwise would be a worse deployment than the chart already gives you.
package main

import (
	"context"
	"flag"
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
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	jobdocker "orchestrator/internal/job/docker"
	"orchestrator/internal/observability"
	"orchestrator/internal/sandbox"
	sandboxdocker "orchestrator/internal/sandbox/docker"
	"orchestrator/internal/server"
	"orchestrator/internal/workload"
	"os"
	"strings"
	"time"
)

func main() {
	var checkReady bool
	flag.BoolVar(&checkReady, "check-ready", false, "exit 0 if the API reports ready, 1 otherwise")
	flag.Parse()

	// Probe path for container healthchecks: the image carries no shell or
	// curl, so the binary probes itself. Stays silent (no log setup).
	if checkReady {
		if ready(config.GetEnv("PORT", "8080")) {
			os.Exit(0)
		}
		os.Exit(1)
	}

	ctx := context.Background()
	svcCfg := config.LoadServiceConfig()
	svcCfg.JobSidecarImage = configuredSidecarImage("JOB_SIDECAR_IMAGE", "job-sidecar")

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "orchestrator", "backend", "docker"))

	if backend := config.GetEnv("ORCHESTRATOR_BACKEND", "docker"); backend != "docker" {
		slog.Error("The all-in-one orchestrator runs the Docker backend only; on Kubernetes run the per-service images from the Helm chart", "backend", backend)
		os.Exit(1)
	}

	// One listener carries both data planes, so the hostname has to say which
	// plane a request is for: the domains must differ. Hence the derived
	// default, and hence refusing to start when they are set the same — a
	// deployment named "s-foo" under the sandbox domain would be shadowed by
	// the sandbox grammar and 404 on its own URL, which is worse than not
	// booting.
	deploymentsDomain := config.GetEnv("DEPLOYMENTS_DOMAIN", "localhost")
	sandboxDomain := config.GetEnv("SANDBOX_DOMAIN", "sandbox."+deploymentsDomain)
	if strings.EqualFold(sandboxDomain, deploymentsDomain) {
		slog.Error("SANDBOX_DOMAIN and DEPLOYMENTS_DOMAIN must differ: one listener serves both data planes and tells them apart by host",
			"domain", sandboxDomain)
		os.Exit(1)
	}

	metrics, err := observability.NewMetrics(ctx)
	if err != nil {
		slog.Error("Failed to init metrics", "error", err)
		os.Exit(1)
	}

	// One dispatcher serves every plane's callbacks — job events, activation
	// results — so there is one queue to drain on shutdown.
	eventDispatcher := dispatcher.NewMemory(dispatcher.LoadConfigFromEnv(), metrics)
	if err := metrics.ObserveInt64("dispatcher_queue_size",
		"Current number of events in dispatcher queue (saturation)",
		eventDispatcher.QueueSize,
	); err != nil {
		slog.Error("Failed to register dispatcher gauge", "error", err)
		os.Exit(1)
	}

	jobs, err := startJobs(ctx, svcCfg, eventDispatcher, metrics)
	if err != nil {
		slog.Error("Failed to start jobs plane", "error", err)
		os.Exit(1)
	}
	defer jobs.orchestrator.Close()

	deployments, err := startDeployments(ctx, deploymentsDomain, eventDispatcher, metrics)
	if err != nil {
		slog.Error("Failed to start deployments plane", "error", err)
		os.Exit(1)
	}
	defer deployments.orchestrator.Close()

	sandboxes, err := startSandboxes(ctx, sandboxDomain, metrics)
	if err != nil {
		slog.Error("Failed to start sandboxes plane", "error", err)
		os.Exit(1)
	}
	defer sandboxes.orchestrator.Close()

	healthChecker := health.NewChecker(jobs.orchestrator, deployments.orchestrator, sandboxes.orchestrator)
	router := api.NewOrchestratorRouter(api.OrchestratorRouterConfig{
		Metrics:           metrics,
		HealthChecker:     healthChecker,
		APIKey:            svcCfg.APIKey,
		JobService:        jobs.service,
		ArtifactEmitter:   jobs.artifacts,
		DeploymentService: deployments.service,
		SandboxService:    sandboxes.service,
	})

	if svcCfg.APIKey == "" {
		slog.Warn("API authentication disabled (including the internal artifact endpoint) - no API_KEY configured")
	}

	// One data listener carries both data planes: sandbox hosts (s-{token}.…)
	// go to the sandbox proxy, everything else to the deployments activator.
	// It keeps a sandbox URL and a deployment URL on the same port a developer
	// already published.
	//
	// The token must look like one for the sandbox proxy to claim the host: a
	// deployment may declare any host it likes, including one under the sandbox
	// domain, and only a minted token can actually address a sandbox. Without
	// that check "s-foo.sandbox.localhost" is swallowed here and 404s on its
	// own URL — the gateway gives the specific host precedence over a wildcard,
	// and this listener has to make the same call.
	sandboxHosts := sandbox.Addressing{Domain: sandboxDomain}
	dataServer := &http.Server{
		Addr: ":" + config.GetEnv("DATA_PORT", "8081"),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token, _, ok := sandboxHosts.Resolve(r.Host); ok && sandbox.IsToken(token) {
				sandboxes.proxy.ServeHTTP(w, r)
				return
			}
			deployments.activator.ServeHTTP(w, r)
		}),
		// Requests can legitimately run for minutes — the per-request timeout
		// lives in the workload-sidecar — so no WriteTimeout here.
		ReadHeaderTimeout: 10 * time.Second,
	}

	if err := server.Serve(ctx, server.Options{
		Handler:           router,
		Port:              svcCfg.Port,
		Extra:             []*http.Server{dataServer},
		DrainWait:         svcCfg.ShutdownDrainWait,
		SetDraining:       healthChecker.SetShuttingDown,
		TelemetryShutdown: metrics.Shutdown,
		Cleanup: func(cleanupCtx context.Context) {
			slog.Info("Draining callback dispatcher")
			if err := eventDispatcher.Close(cleanupCtx); err != nil {
				slog.Warn("Dispatcher shutdown error", "error", err)
			}
			slog.Info("Running workloads will continue independently")
		},
	}); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

// jobsPlane is the jobs control plane's pieces the router and shutdown need.
type jobsPlane struct {
	orchestrator job.Orchestrator
	service      *job.Service
	artifacts    api.ArtifactEmitter
}

func startJobs(ctx context.Context, svcCfg *config.ServiceConfig, queue dispatcher.Queue, metrics *observability.Metrics) (*jobsPlane, error) {
	cfg := jobdocker.LoadConfigFromEnv()
	factory := jobdocker.NewOrchestrator(ctx, jobdocker.Config{
		SidecarImage:        svcCfg.JobSidecarImage,
		RetentionPeriod:     cfg.JobRetention,
		MaintenanceInterval: cfg.MaintenanceInterval,
		ArtifactEndpoint:    cfg.ArtifactEndpoint,
		ExtraHosts:          cfg.ExtraHosts,
		Network:             cfg.Network,
	})

	orchestrator, err := job.NewOrchestrator(server.NewJobEmitter(queue, metrics), factory)
	if err != nil {
		return nil, err
	}
	if counter, ok := orchestrator.(interface{ ActiveJobs() int64 }); ok {
		if err := metrics.ObserveInt64("jobs_active",
			"Jobs currently in flight on this replica (saturation)",
			counter.ActiveJobs,
		); err != nil {
			return nil, err
		}
	}
	if err := orchestrator.Start(ctx); err != nil {
		return nil, err
	}

	plane := &jobsPlane{
		orchestrator: orchestrator,
		service:      job.NewService(orchestrator, metrics, artifact.DefaultRegistry(), svcCfg.APIKey),
	}
	if emitter, ok := orchestrator.(api.ArtifactEmitter); ok {
		plane.artifacts = emitter
	}
	return plane, nil
}

// deploymentsPlane is the serving plane plus its in-process data plane.
type deploymentsPlane struct {
	orchestrator deployment.Orchestrator
	service      *deployment.Service
	activator    *activator.Activator
}

func startDeployments(ctx context.Context, domain string, queue dispatcher.Queue, metrics *observability.Metrics) (*deploymentsPlane, error) {
	dataPort := config.GetEnv("DATA_PORT", "8081")

	cfg := depdocker.LoadConfigFromEnv()
	cfg.SidecarImage = configuredSidecarImage("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar")
	orchestrator, err := depdocker.NewOrchestrator(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := orchestrator.Start(ctx); err != nil {
		return nil, err
	}

	svc := deployment.NewService(orchestrator, metrics, artifact.MountingRegistry(), domain,
		func(host string) string {
			if dataPort == "80" {
				return "http://" + host
			}
			return "http://" + host + ":" + dataPort
		})

	// The activator is on-path for all deployment traffic, so it is itself the
	// cold-start queue signal the autoscaler reads.
	act := activator.New(svc, queue, metrics)
	scaler := autoscaler.New(orchestrator,
		autoscaler.NewSidecarConcurrency(orchestrator, workload.DefaultAdminPort),
		autoscaler.QueuedDepthFunc(act.QueuedDepth),
		autoscaler.LoadConfigFromEnv(), metrics)
	go scaler.Run(ctx)

	return &deploymentsPlane{orchestrator: orchestrator, service: svc, activator: act}, nil
}

// sandboxesPlane is the sandbox control plane plus its in-process data plane.
type sandboxesPlane struct {
	orchestrator sandbox.Orchestrator
	service      *sandbox.Service
	proxy        *activator.SandboxProxy
}

func startSandboxes(ctx context.Context, domain string, metrics *observability.Metrics) (*sandboxesPlane, error) {
	pools, err := sandbox.LoadPools(config.GetEnv("SANDBOX_POOLS_JSON", ""))
	if err != nil {
		return nil, fmt.Errorf("invalid sandbox pool configuration: %w", err)
	}

	cfg := sandboxdocker.LoadConfigFromEnv()
	cfg.SidecarImage = configuredSidecarImage("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar")
	cfg.Pools = pools
	// The domain the URLs are minted under must be the one the proxy resolves.
	cfg.SandboxDomain = domain
	orchestrator, err := sandboxdocker.NewOrchestrator(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := orchestrator.Start(ctx); err != nil {
		return nil, err
	}

	proxy := activator.NewSandboxProxy(orchestrator, activator.SandboxConfig{
		Domain: domain,
		Hold:   time.Duration(config.GetIntEnv("SANDBOX_HOLD_SECONDS", 5)) * time.Second,
	}, metrics)

	return &sandboxesPlane{
		orchestrator: orchestrator,
		service:      sandbox.NewService(orchestrator, metrics, pools, artifact.MountingRegistry()),
		proxy:        proxy,
	}, nil
}

// ready reports whether the API on the given local port answers /readyz.
func ready(port string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
