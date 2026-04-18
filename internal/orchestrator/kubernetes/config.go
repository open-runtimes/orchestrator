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
	LeaderElection                LeaderElectionConfig
}

// LeaderElectionConfig controls how replicas coordinate so that exactly one of
// them runs the lifecycle watcher and emits callbacks. HTTP reads/writes are
// always handled by any replica — only the watcher is leader-gated.
type LeaderElectionConfig struct {
	Enabled        bool          // when false, Start runs the watcher directly (single-replica mode)
	LeaseName      string        // Lease resource name in the configured namespace
	Identity       string        // unique per-replica string (usually Pod name)
	LeaseDuration  time.Duration // how long non-leaders wait before taking over after a failed renewal
	RenewDeadline  time.Duration // how long the leader retries renewing before giving up
	RetryPeriod    time.Duration // how often non-leaders try to acquire the lease
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
		LeaderElection: LeaderElectionConfig{
			Enabled:       config.GetEnv("KUBE_LEADER_ELECTION", "") == "true",
			LeaseName:     config.GetEnv("KUBE_LEADER_LEASE_NAME", "jobs-service-leader"),
			Identity:      config.GetEnv("KUBE_LEADER_IDENTITY", ""),
			LeaseDuration: config.GetDurationEnv("KUBE_LEADER_LEASE_DURATION", 15*time.Second),
			RenewDeadline: config.GetDurationEnv("KUBE_LEADER_RENEW_DEADLINE", 10*time.Second),
			RetryPeriod:   config.GetDurationEnv("KUBE_LEADER_RETRY_PERIOD", 2*time.Second),
		},
	}
}
