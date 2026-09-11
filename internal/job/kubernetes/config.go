package kubernetes

import (
	"orchestrator/internal/config"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Config holds configuration for the Kubernetes orchestrator.
type Config struct {
	SidecarImage                  string
	Kubeconfig                    string
	Context                       string // kubeconfig context to pin; empty uses current-context
	Namespace                     string
	ServiceAccount                string
	ImagePullSecrets              []string
	WorkerImagePullPolicy         string // applied to the worker (user) container; empty = kubelet default
	SidecarImagePullPolicy        string // applied to the combined sidecar; empty = kubelet default
	JobRetention                  time.Duration
	LogFlushInterval              time.Duration // max time buffered job log lines wait before a callback flush
	ArtifactEndpoint              string
	TerminationGracePeriodSeconds int64 // grace period for the combined sidecar to run post-artifacts
	LeaderElection                LeaderElectionConfig

	// Overcommit derives worker requests from declared limits (internal/kube).
	Overcommit kube.Overcommit
	// Tolerations are stamped on every job pod (internal/kube).
	Tolerations []corev1.Toleration
	// NodeSelector pins every job pod to a node pool (internal/kube).
	NodeSelector map[string]string
	// Metrics wires backend-specific recorders (leadership, status cache,
	// tracker saturation, K8s API latency). Optional — when nil, recording
	// is skipped.
	Metrics *observability.Metrics
}

// LeaderElectionConfig coordinates replicas so exactly one runs the lifecycle
// watcher; shared with other services via internal/kube.
type LeaderElectionConfig = kube.LeaderElectionConfig

// LoadConfigFromEnv loads orchestrator configuration from environment variables.
// SidecarImage and Metrics are service-level and are set by the caller.
func LoadConfigFromEnv() (Config, error) {
	var pullSecrets []string
	if secrets := config.GetEnv("KUBE_IMAGE_PULL_SECRETS", ""); secrets != "" {
		pullSecrets = strings.Split(secrets, ",")
	}
	tolerations, err := kube.TolerationsFromEnv()
	if err != nil {
		return Config{}, err
	}
	nodeSelector, err := kube.NodeSelectorFromEnv()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Kubeconfig:                    config.GetEnv("KUBECONFIG", ""),
		Context:                       config.GetEnv("KUBE_CONTEXT", ""),
		Namespace:                     config.GetEnv("KUBE_NAMESPACE", "orchestrator"),
		ServiceAccount:                config.GetEnv("KUBE_JOB_SERVICE_ACCOUNT", "job-sidecar"),
		ImagePullSecrets:              pullSecrets,
		WorkerImagePullPolicy:         config.GetEnv("KUBE_WORKER_IMAGE_PULL_POLICY", ""),
		SidecarImagePullPolicy:        config.GetEnv("KUBE_SIDECAR_IMAGE_PULL_POLICY", ""),
		JobRetention:                  config.GetDurationEnv("JOB_RETENTION", 15*time.Minute),
		LogFlushInterval:              config.GetDurationEnv("KUBE_LOG_FLUSH_INTERVAL", 1*time.Second),
		ArtifactEndpoint:              config.GetEnv("ARTIFACT_ENDPOINT", "http://jobs-service.orchestrator.svc.cluster.local:8080"),
		TerminationGracePeriodSeconds: int64(config.GetIntEnv("KUBE_TERMINATION_GRACE_SECONDS", 600)),
		Overcommit:                    kube.OvercommitFromEnv(),
		Tolerations:                   tolerations,
		NodeSelector:                  nodeSelector,
		LeaderElection: LeaderElectionConfig{
			Enabled:       config.GetEnv("KUBE_LEADER_ELECTION", "") == "true",
			LeaseName:     config.GetEnv("KUBE_LEADER_LEASE_NAME", ""),
			Identity:      config.GetEnv("KUBE_LEADER_IDENTITY", ""),
			LeaseDuration: config.GetDurationEnv("KUBE_LEADER_LEASE_DURATION", 0),
			RenewDeadline: config.GetDurationEnv("KUBE_LEADER_RENEW_DEADLINE", 0),
			RetryPeriod:   config.GetDurationEnv("KUBE_LEADER_RETRY_PERIOD", 0),
		},
	}
	cfg.LeaderElection.ApplyDefaults("jobs-service-leader")
	return cfg, nil
}
