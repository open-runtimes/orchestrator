// jobs-service is the HTTP API server for managing container jobs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/api"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
	"orchestrator/internal/orchestrator/docker"
	"orchestrator/internal/orchestrator/kubernetes"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// buildOrchestratorFactory returns the appropriate OrchestratorFactory for the
// configured backend. The caller wires in the shared CallbackEmitter via
// job.NewOrchestrator.
func buildOrchestratorFactory(ctx context.Context, backend, sidecarImage string) (job.OrchestratorFactory, error) {
	switch backend {
	case "docker":
		cfg := docker.LoadConfigFromEnv()
		return docker.NewOrchestrator(ctx, docker.Config{
			SidecarImage:        sidecarImage,
			RetentionPeriod:     cfg.JobRetention,
			MaintenanceInterval: cfg.MaintenanceInterval,
			ArtifactEndpoint:    cfg.ArtifactEndpoint,
			ExtraHosts:          cfg.ExtraHosts,
		}), nil
	case "kubernetes":
		cfg := kubernetes.LoadConfigFromEnv()
		return kubernetes.NewOrchestrator(ctx, kubernetes.Config{
			SidecarImage:                  sidecarImage,
			Kubeconfig:                    cfg.Kubeconfig,
			Namespace:                     cfg.Namespace,
			ServiceAccount:                cfg.ServiceAccount,
			ImagePullSecrets:              cfg.ImagePullSecrets,
			RetentionPeriod:               cfg.JobRetention,
			MaintenanceInterval:           cfg.MaintenanceInterval,
			ArtifactEndpoint:              cfg.ArtifactEndpoint,
			TerminationGracePeriodSeconds: cfg.TerminationGracePeriodSeconds,
		}), nil
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// Load configuration
	svcCfg := config.LoadServiceConfig()
	dispatcherCfg := dispatcher.LoadConfigFromEnv()
	backend := config.GetEnv("ORCHESTRATOR_BACKEND", "docker")

	// Setup metrics
	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		return err
	}

	// Create callback dispatcher and event emitter
	eventDispatcher := dispatcher.NewMemory(dispatcherCfg, metrics)
	emitter := job.NewCallbackEmitter()
	emitter.Register(func(e *job.CallbackEnvelope) {
		if e.CallbackURL == "" {
			return
		}
		if err := eventDispatcher.Dispatch(&dispatcher.Event{
			Payload:     e.Payload,
			Destination: e.CallbackURL,
			SigningKey:  e.SigningKey,
		}); err != nil {
			slog.Warn("Failed to dispatch job event", "type", e.Payload.Type, "error", err)
		}
	})
	emitter.Register(func(e *job.CallbackEnvelope) {
		if metrics == nil || e.Payload == nil || e.Payload.Type != job.CallbackTypeExit {
			return
		}
		image, _ := e.Payload.Data["image"].(string)
		exitCode := -1
		if code, ok := e.Payload.Data["exitCode"].(int); ok {
			exitCode = code
		}
		var duration float64
		if d, ok := e.Payload.Data["durationSeconds"].(float64); ok {
			duration = d
		}
		metrics.RecordJobCompleted(context.Background(), image, exitCode == 0, duration)
	})

	// Create orchestrator for the configured backend.
	factory, err := buildOrchestratorFactory(ctx, backend, svcCfg.SidecarImage)
	if err != nil {
		return err
	}
	orchestrator, err := job.NewOrchestrator(emitter, factory)
	if err != nil {
		return err
	}
	defer orchestrator.Close()

	// Start reconciliation and maintenance
	if err := orchestrator.Start(ctx); err != nil {
		return err
	}

	slog.Info("Orchestrator ready", "backend", backend)

	// Create health checker
	healthChecker := health.NewChecker(orchestrator)

	// Create job service
	jobService := job.NewService(orchestrator, metrics, artifact.DefaultRegistry())

	// Create API router
	routerCfg := api.RouterConfig{
		JobService:    jobService,
		Metrics:       metrics,
		HealthChecker: healthChecker,
		APIKey:        svcCfg.APIKey,
	}
	if ae, ok := orchestrator.(api.ArtifactEmitter); ok {
		routerCfg.ArtifactEmitter = ae
	}
	router := api.NewRouter(routerCfg)

	if svcCfg.APIKey != "" {
		slog.Info("API authentication enabled")
	} else {
		slog.Warn("API authentication disabled - no API_KEY configured")
	}

	// Create API server
	apiServer := &http.Server{
		Addr:         ":" + svcCfg.Port,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Create metrics server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metricsHandler)
	metricsServer := &http.Server{
		Addr:         ":" + svcCfg.MetricsPort,
		Handler:      metricsMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Channel to capture server errors
	serverErr := make(chan error, 1)

	// Start API server
	go func() {
		slog.Info("Starting API server", "port", svcCfg.Port)
		if err := apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Start metrics server
	go func() {
		slog.Info("Starting metrics server", "port", svcCfg.MetricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// shutdown closes both servers gracefully
	shutdown := func(timeout time.Duration) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := apiServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("API server shutdown error", "error", err)
		}
		if err := metricsServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Metrics server shutdown error", "error", err)
		}
	}

	// Wait for interrupt signal or server error
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("Received shutdown signal", "signal", sig)
	case err := <-serverErr:
		slog.Error("Server failed to start", "error", err)
		shutdown(5 * time.Second)
		return err
	}

	// Phase 1: Mark service as unhealthy for load balancer draining
	healthChecker.SetShuttingDown()

	// Wait for load balancers to stop sending traffic
	if svcCfg.ShutdownDrainWait > 0 {
		slog.Info("Waiting for traffic to drain", "duration", svcCfg.ShutdownDrainWait)
		time.Sleep(svcCfg.ShutdownDrainWait)
	}

	// Phase 2: Graceful shutdown - stop accepting new connections, finish in-flight requests
	slog.Info("Starting graceful shutdown")
	shutdown(25 * time.Second)

	// Phase 3: Drain callback dispatcher
	slog.Info("Draining callback dispatcher")
	dispatcherCtx, dispatcherCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dispatcherCancel()
	if err := eventDispatcher.Close(dispatcherCtx); err != nil {
		slog.Warn("Dispatcher shutdown error", "error", err)
	}

	// Log final dispatcher stats
	stats := eventDispatcher.Stats()
	slog.Info("Dispatcher stats",
		"delivered", stats.Delivered,
		"failed", stats.Failed,
		"dropped", stats.Dropped,
	)

	// Jobs continue running in orchestrator - they're self-contained (container + sidecar)
	// and don't need the service. They will complete and send callbacks as configured.
	slog.Info("Running jobs will continue independently")
	slog.Info("Shutdown complete")
	return nil
}
