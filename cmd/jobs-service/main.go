// jobs-service is the HTTP API server for managing container jobs.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"orchestrator/internal/config"
	"orchestrator/internal/observability"
	"orchestrator/internal/orchestrator/docker"
	"orchestrator/internal/orchestrator/kubernetes"
	"orchestrator/pkg/job"
	"orchestrator/pkg/server"
	"os"
)

func main() {
	ctx := context.Background()
	svcCfg := config.LoadServiceConfig()
	backend := config.GetEnv("ORCHESTRATOR_BACKEND", "docker")

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("backend", backend))

	// Metrics live at the top of main so the same instance is shared by both
	// the backend factory (for backend-specific recorders) and server.Run
	// (for HTTP / dispatcher recorders and the /metrics handler).
	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		slog.Error("Failed to init metrics", "error", err)
		os.Exit(1)
	}

	factory, err := buildOrchestratorFactory(ctx, backend, svcCfg.JobSidecarImage, metrics)
	if err != nil {
		slog.Error("Failed to build orchestrator factory", "error", err)
		os.Exit(1)
	}

	if err := server.Run(ctx, factory, metrics, metricsHandler); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

// buildOrchestratorFactory selects a backend. When the Kubernetes backend moves
// to a private module, this function in the public main shrinks to just the
// docker case; the private repo supplies its own main.go that wires kubernetes.
func buildOrchestratorFactory(ctx context.Context, backend, sidecarImage string, metrics *observability.Metrics) (job.OrchestratorFactory, error) {
	switch backend {
	case "docker":
		cfg := docker.LoadConfigFromEnv()
		return docker.NewOrchestrator(ctx, docker.Config{
			SidecarImage:        sidecarImage,
			RetentionPeriod:     cfg.JobRetention,
			MaintenanceInterval: cfg.MaintenanceInterval,
			ArtifactEndpoint:    cfg.ArtifactEndpoint,
			ExtraHosts:          cfg.ExtraHosts,
			Network:             cfg.Network,
		}), nil
	case "kubernetes":
		cfg, err := kubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return kubernetes.NewOrchestrator(ctx, kubernetes.Config{
			SidecarImage:                  sidecarImage,
			Kubeconfig:                    cfg.Kubeconfig,
			Context:                       cfg.Context,
			Namespace:                     cfg.Namespace,
			ServiceAccount:                cfg.ServiceAccount,
			ImagePullSecrets:              cfg.ImagePullSecrets,
			WorkerImagePullPolicy:         cfg.WorkerImagePullPolicy,
			SidecarImagePullPolicy:        cfg.SidecarImagePullPolicy,
			RetentionPeriod:               cfg.JobRetention,
			MaintenanceInterval:           cfg.MaintenanceInterval,
			LogFlushInterval:              cfg.LogFlushInterval,
			ArtifactEndpoint:              cfg.ArtifactEndpoint,
			TerminationGracePeriodSeconds: cfg.TerminationGracePeriodSeconds,
			LeaderElection:                cfg.LeaderElection,
			Overcommit:                    cfg.Overcommit,
			Tolerations:                   cfg.Tolerations,
			Metrics:                       metrics,
		}), nil
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}
