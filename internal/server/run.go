// Package server wires the orchestrator components together and runs the HTTP
// service until it receives a shutdown signal. Serve is the generic core
// (HTTP servers + graceful shutdown); Run adds the jobs-service wiring.
package server

import (
	"context"
	"log/slog"
	"orchestrator/internal/api"
	"orchestrator/internal/artifact"
	"orchestrator/internal/config"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"orchestrator/internal/observability"
)

// RegisterJobListeners wires the job callback emitter: every job event with a
// callback URL goes to the dispatcher, and the completion metrics are recorded
// off the exit event. Exported because the all-in-one orchestrator binary wires
// the jobs plane itself, and this is the part that must not drift between them.
func RegisterJobListeners(emitter *job.CallbackEmitter, queue dispatcher.Queue, metrics *observability.Metrics) {
	emitter.Register(func(e *job.CallbackEnvelope) {
		if e.CallbackURL == "" {
			return
		}
		if err := queue.Dispatch(&dispatcher.Event{
			Payload:     e.Payload,
			Destination: e.CallbackURL,
			SigningKey:  e.SigningKey,
		}); err != nil {
			slog.Warn("Failed to dispatch job event", "type", e.Payload.Type, "error", err)
		}
	})
	emitter.Register(func(e *job.CallbackEnvelope) {
		if exit, ok := e.Payload.Data.(job.ExitData); ok {
			metrics.RecordJobCompleted(context.Background(), exit.Image, exit.ExitCode == 0, exit.DurationSeconds)
		}
	})
}

// Run bootstraps the jobs service around orchestrator and blocks until
// SIGINT/SIGTERM or a server error. It returns nil on a clean shutdown.
//
// emitter is the one the backend was built with; Run registers its listeners
// before Start. metrics must be the same instance the backend was built
// against so recorders on both sides share the same meter. Config is loaded
// from environment variables by the various internal packages; add attributes
// to slog.Default before calling Run if you want them attached to every log
// line.
func Run(ctx context.Context, orchestrator job.Orchestrator, emitter *job.CallbackEmitter, metrics *observability.Metrics) error {
	svcCfg := config.LoadServiceConfig()

	eventDispatcher := dispatcher.NewMemory(dispatcher.LoadConfigFromEnv(), metrics)
	RegisterJobListeners(emitter, eventDispatcher, metrics)

	if err := metrics.ObserveInt64("dispatcher_queue_size",
		"Current number of events in dispatcher queue (saturation)",
		eventDispatcher.QueueSize,
	); err != nil {
		return err
	}

	defer orchestrator.Close()

	if err := metrics.ObserveInt64("jobs_active",
		"Jobs currently in flight on this replica (saturation)",
		orchestrator.ActiveJobs,
	); err != nil {
		return err
	}

	if err := orchestrator.Start(ctx); err != nil {
		return err
	}
	slog.Info("Orchestrator ready")

	healthChecker := health.NewChecker(orchestrator)
	jobService := job.NewService(orchestrator, metrics, artifact.DefaultRegistry(), svcCfg.APIKey)

	router := api.NewOrchestratorRouter(api.OrchestratorRouterConfig{
		JobService:    jobService,
		JobCallbacks:  emitter,
		Metrics:       metrics,
		HealthChecker: healthChecker,
		APIKey:        svcCfg.APIKey,
	})

	if svcCfg.APIKey != "" {
		slog.Info("API authentication enabled")
	} else {
		slog.Warn("API authentication disabled (including the internal artifact endpoint) - no API_KEY configured")
	}

	return Serve(ctx, Options{
		Handler:           router,
		Port:              svcCfg.Port,
		DrainWait:         svcCfg.ShutdownDrainWait,
		SetDraining:       healthChecker.SetShuttingDown,
		TelemetryShutdown: metrics.Shutdown,
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
