// jobs-service is the HTTP API server for managing container jobs.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"orchestrator/internal/config"
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

	factory, err := buildOrchestratorFactory(ctx, backend, svcCfg.SidecarImage)
	if err != nil {
		slog.Error("Failed to build orchestrator factory", "error", err)
		os.Exit(1)
	}

	if err := server.Run(ctx, factory); err != nil {
		slog.Error("Service failed", "error", err)
		os.Exit(1)
	}
}

// buildOrchestratorFactory selects a backend. When the Kubernetes backend moves
// to a private module, this function in the public main shrinks to just the
// docker case; the private repo supplies its own main.go that wires kubernetes.
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
			WorkerImagePullPolicy:         cfg.WorkerImagePullPolicy,
			SidecarImagePullPolicy:        cfg.SidecarImagePullPolicy,
			RetentionPeriod:               cfg.JobRetention,
			MaintenanceInterval:           cfg.MaintenanceInterval,
			ArtifactEndpoint:              cfg.ArtifactEndpoint,
			TerminationGracePeriodSeconds: cfg.TerminationGracePeriodSeconds,
			LeaderElection:                cfg.LeaderElection,
		}), nil
	default:
		return nil, fmt.Errorf("unknown orchestrator backend %q (expected docker|kubernetes)", backend)
	}
}
