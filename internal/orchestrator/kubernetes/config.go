package kubernetes

import (
	"orchestrator/internal/config"
	"strings"
	"time"
)

// OrchestratorConfig holds configuration for the Kubernetes orchestrator.
type OrchestratorConfig struct {
	Kubeconfig                    string
	Namespace                     string
	ServiceAccount                string
	ImagePullSecrets              []string
	WorkerImagePullPolicy         string // applied to the worker (user) container; empty = kubelet default
	SidecarImagePullPolicy        string // applied to artifact-pre + artifact-post; empty = kubelet default
	JobRetention                  time.Duration
	MaintenanceInterval           time.Duration
	ArtifactEndpoint              string
	TerminationGracePeriodSeconds int64 // grace period for post-sidecar to run post-artifacts
}

// LoadConfigFromEnv loads orchestrator configuration from environment variables.
func LoadConfigFromEnv() OrchestratorConfig {
	var pullSecrets []string
	if secrets := config.GetEnv("KUBE_IMAGE_PULL_SECRETS", ""); secrets != "" {
		pullSecrets = strings.Split(secrets, ",")
	}
	return OrchestratorConfig{
		Kubeconfig:                    config.GetEnv("KUBECONFIG", ""),
		Namespace:                     config.GetEnv("KUBE_NAMESPACE", "orchestrator"),
		ServiceAccount:                config.GetEnv("KUBE_JOB_SERVICE_ACCOUNT", "job-sidecar"),
		ImagePullSecrets:              pullSecrets,
		WorkerImagePullPolicy:         config.GetEnv("KUBE_WORKER_IMAGE_PULL_POLICY", ""),
		SidecarImagePullPolicy:        config.GetEnv("KUBE_SIDECAR_IMAGE_PULL_POLICY", ""),
		JobRetention:                  config.GetDurationEnv("JOB_RETENTION", 15*time.Minute),
		MaintenanceInterval:           config.GetDurationEnv("MAINTENANCE_INTERVAL", 1*time.Minute),
		ArtifactEndpoint:              config.GetEnv("ARTIFACT_ENDPOINT", "http://jobs-service.orchestrator.svc.cluster.local:8080"),
		TerminationGracePeriodSeconds: int64(config.GetIntEnv("KUBE_TERMINATION_GRACE_SECONDS", 600)),
	}
}
