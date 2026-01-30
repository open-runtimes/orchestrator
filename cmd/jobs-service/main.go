// jobs-service is the HTTP API server for managing container jobs.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"orchestrator/internal/api"
	"orchestrator/internal/config"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
	"orchestrator/internal/orchestrator/docker"
	"os"
	"os/signal"
	"syscall"
	"time"
)

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
	orchCfg := docker.LoadConfigFromEnv()
	dispatcherCfg := dispatcher.LoadConfigFromEnv()

	// Setup metrics
	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		return err
	}

	// Create callback dispatcher
	eventDispatcher := dispatcher.NewMemory(dispatcherCfg, metrics)

	// Create Docker orchestrator (automatically reconciles existing jobs)
	orchestrator, err := docker.NewOrchestrator(ctx, docker.Config{
		SidecarImage:        svcCfg.SidecarImage,
		RetentionPeriod:     orchCfg.JobRetention,
		MaintenanceInterval: orchCfg.MaintenanceInterval,
		Dispatcher:          eventDispatcher,
		CallbackProxyURL:    orchCfg.CallbackProxyURL,
		ExtraHosts:          orchCfg.ExtraHosts,
		Metrics:             metrics,
	})
	if err != nil {
		return err
	}
	defer orchestrator.Close()

	slog.Info("Connected to Docker daemon")

	// Create health checker
	healthChecker := health.NewChecker(orchestrator)

	// Create job service
	jobService := job.NewService(orchestrator, metrics)

	// Create API router
	router := api.NewRouter(api.RouterConfig{
		JobService:    jobService,
		Metrics:       metrics,
		HealthChecker: healthChecker,
		Dispatcher:    eventDispatcher,
		APIKey:        svcCfg.APIKey,
	})

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
