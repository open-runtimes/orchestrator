// jobs-service is the HTTP API server for managing container jobs.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"orchestrator/internal/config"
	"orchestrator/internal/job"
	"orchestrator/internal/job/docker"
	"orchestrator/internal/job/kubernetes"
	"orchestrator/internal/observability"
	"orchestrator/internal/server"
	"os"
)

func main() {
	ctx := context.Background()
	svcCfg := config.LoadServiceConfig()
	backend := config.GetEnv("ORCHESTRATOR_BACKEND", "docker")

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("backend", backend))

	// Metrics live at the top of main so the same instance is shared by the
	// backend and server.Run's HTTP and dispatcher recorders.
	metrics, err := observability.NewMetrics(ctx)
	if err != nil {
		slog.Error("Failed to init metrics", "error", err)
		os.Exit(1)
	}

	emitter := &job.CallbackEmitter{}
	orchestrator, err := newOrchestrator(backend, svcCfg.JobSidecarImage, emitter, metrics)
	if err != nil {
		slog.Error("Failed to build orchestrator", "error", err)
		os.Exit(1)
	}

	if err := server.Run(ctx, orchestrator, emitter, metrics); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

// newOrchestrator selects a backend. When the Kubernetes backend moves to a
// private module, this function in the public main shrinks to just the docker
// case; the private repo supplies its own main.go that wires kubernetes.
func newOrchestrator(backend, sidecarImage string, emitter *job.CallbackEmitter, metrics *observability.Metrics) (job.Orchestrator, error) {
	switch backend {
	case "docker":
		cfg := docker.LoadConfigFromEnv()
		cfg.SidecarImage = sidecarImage
		return docker.NewOrchestrator(cfg, emitter)
	case "kubernetes":
		cfg, err := kubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, err
		}
		cfg.SidecarImage = sidecarImage
		cfg.Metrics = metrics
		return kubernetes.NewOrchestrator(cfg, emitter)
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}
