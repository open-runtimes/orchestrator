// Package server wires the orchestrator components together and runs the HTTP
// service until it receives a shutdown signal. The caller is only responsible
// for choosing a backend and supplying its OrchestratorFactory — everything
// else (config, dispatcher, API, metrics, graceful shutdown) lives here.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"orchestrator/internal/api"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/pkg/job"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
)

// Run bootstraps the orchestrator service against the supplied backend factory
// and blocks until SIGINT/SIGTERM or a server error. It returns nil on a clean
// shutdown.
//
// The factory provides the chosen backend (Docker, Kubernetes, or any other
// implementation of job.Orchestrator). metrics must be the same instance the
// factory was built against so recorders on both sides share the same meter.
// Config is loaded from environment variables by the various internal
// packages; add attributes to slog.Default before calling Run if you want
// them attached to every log line.
func Run(ctx context.Context, factory job.OrchestratorFactory, metrics *observability.Metrics, metricsHandler http.Handler) error {
	// Route client-go / leaderelection / apimachinery logs (which go through
	// klog) via slog, so everything in the container's stdout is one ndjson
	// stream. Must run before any klog-using library is invoked.
	klog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	svcCfg := config.LoadServiceConfig()
	dispatcherCfg := dispatcher.LoadConfigFromEnv()

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
			Headers:     e.Headers,
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

	orchestrator, err := job.NewOrchestrator(emitter, factory)
	if err != nil {
		return err
	}
	defer orchestrator.Close()

	if err := orchestrator.Start(ctx); err != nil {
		return err
	}
	slog.Info("Orchestrator ready")

	healthChecker := health.NewChecker(orchestrator)
	jobService := job.NewService(orchestrator, metrics, artifact.DefaultRegistry())

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

	apiServer := &http.Server{
		Addr:         ":" + svcCfg.Port,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metricsHandler)
	metricsServer := &http.Server{
		Addr:         ":" + svcCfg.MetricsPort,
		Handler:      metricsMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)

	go func() {
		slog.Info("Starting API server", "port", svcCfg.Port)
		if err := apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	go func() {
		slog.Info("Starting metrics server", "port", svcCfg.MetricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

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

	// Phase 1: drain load balancer traffic.
	healthChecker.SetShuttingDown()
	if svcCfg.ShutdownDrainWait > 0 {
		slog.Info("Waiting for traffic to drain", "duration", svcCfg.ShutdownDrainWait)
		time.Sleep(svcCfg.ShutdownDrainWait)
	}

	// Phase 2: graceful shutdown of HTTP servers.
	slog.Info("Starting graceful shutdown")
	shutdown(25 * time.Second)

	// Phase 3: drain callback dispatcher.
	slog.Info("Draining callback dispatcher")
	dispatcherCtx, dispatcherCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dispatcherCancel()
	if err := eventDispatcher.Close(dispatcherCtx); err != nil {
		slog.Warn("Dispatcher shutdown error", "error", err)
	}

	stats := eventDispatcher.Stats()
	slog.Info("Dispatcher stats",
		"delivered", stats.Delivered,
		"failed", stats.Failed,
		"dropped", stats.Dropped,
	)

	// Running jobs continue to run; they're self-contained and will finish and
	// callback on their own timeline.
	slog.Info("Running jobs will continue independently")
	slog.Info("Shutdown complete")
	return nil
}
