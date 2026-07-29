// Package server wires the orchestrator components together and runs the HTTP
// service until it receives a shutdown signal. Serve is the generic core
// (HTTP servers + graceful shutdown); Run adds the jobs-service wiring.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"orchestrator/internal/api"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/observability"
	"orchestrator/pkg/job"
)

// activeCounter is the optional surface a backend implements to report its live
// in-flight job count. Run registers it as the jobs_active async gauge, which is
// why the count must be derived from live state rather than tallied: a +1/-1
// pair split across a create request and an exit callback drifts on every
// restart and every leadership handover.
type activeCounter interface {
	ActiveJobs() int64
}

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

	if err := metrics.ObserveInt64("dispatcher_queue_size",
		"Current number of events in dispatcher queue (saturation)",
		eventDispatcher.QueueSize,
	); err != nil {
		return err
	}

	orchestrator, err := job.NewOrchestrator(emitter, factory)
	if err != nil {
		return err
	}
	defer orchestrator.Close()

	if counter, ok := orchestrator.(activeCounter); ok {
		if err := metrics.ObserveInt64("jobs_active",
			"Jobs currently in flight on this replica (saturation)",
			counter.ActiveJobs,
		); err != nil {
			return err
		}
	}

	if err := orchestrator.Start(ctx); err != nil {
		return err
	}
	slog.Info("Orchestrator ready")

	healthChecker := health.NewChecker(orchestrator)
	jobService := job.NewService(orchestrator, metrics, artifact.DefaultRegistry(), svcCfg.APIKey)

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
		slog.Warn("API authentication disabled (including the internal artifact endpoint) - no API_KEY configured")
	}

	return Serve(ctx, Options{
		Handler:        router,
		MetricsHandler: metricsHandler,
		Port:           svcCfg.Port,
		MetricsPort:    svcCfg.MetricsPort,
		DrainWait:      svcCfg.ShutdownDrainWait,
		SetDraining:    healthChecker.SetShuttingDown,
		Cleanup: func(cleanupCtx context.Context) {
			slog.Info("Draining callback dispatcher")
			if err := eventDispatcher.Close(cleanupCtx); err != nil {
				slog.Warn("Dispatcher shutdown error", "error", err)
			}
			stats := eventDispatcher.Stats()
			slog.Info("Dispatcher stats",
				"delivered", stats.Delivered,
				"failed", stats.Failed,
				"dropped", stats.Dropped,
			)
			// Running jobs continue to run; they're self-contained and will
			// finish and callback on their own timeline.
			slog.Info("Running jobs will continue independently")
		},
	})
}
