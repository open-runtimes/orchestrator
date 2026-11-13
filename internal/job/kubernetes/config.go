package kubernetes

import (
	"log/slog"
	"orchestrator/internal/config"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const defaultRequestTimeout = 20 * time.Second

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
	ClientQPS                     int           // client-side K8s API steady-state QPS; 0 = kube.DefaultClientQPS
	ClientBurst                   int           // client-side K8s API burst; 0 = kube.DefaultClientBurst
	RequestTimeout                time.Duration // bounds the K8s calls behind create, cancel and status; 0 = defaultRequestTimeout

	// Overcommit derives worker requests from declared limits (internal/kube).
	Overcommit kube.Overcommit
	// Tolerations are stamped on every job pod (internal/kube).
	Tolerations []corev1.Toleration
	// NodeSelector pins every job pod to a node pool (internal/kube).
	NodeSelector map[string]string
	// PinNodes is how many nodes a placement key prefers (kube.Pin); 0 is off.
	PinNodes int
	// PodSecurityContext is stamped on every job pod (kube.PodSecurity).
	PodSecurityContext corev1.PodSecurityContext
	// MountAgentPort, when set, has the node mount agent listening on that port
	// perform jobs' artifact mounts in a CSI workspace, so the pod needs no
	// privileged container (docs/design/node-mount-agent.md). Zero mounts from
	// a privileged sidecar in the pod. Jobs without a mount are unaffected.
	// The chart sets it when mountAgent is enabled.
	MountAgentPort int
	// HostUsers keeps job pods in the node's user namespace. False (the
	// default) gives each job pod its own, so a build running as root is an
	// unprivileged id on the node. buildJob keeps the node's for a pod that
	// cannot have one.
	HostUsers bool
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
	podSecurity, err := kube.PodSecurityContextFromEnv()
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
		ArtifactEndpoint:              config.GetEnv("ARTIFACT_ENDPOINT", "http://jobs-service.orchestrator.svc.cluster.local:8082"),
		TerminationGracePeriodSeconds: int64(config.GetIntEnv("KUBE_TERMINATION_GRACE_SECONDS", 600)),
		Overcommit:                    kube.OvercommitFromEnv(),
		Tolerations:                   tolerations,
		NodeSelector:                  nodeSelector,
		PinNodes:                      config.GetIntEnv("KUBE_WORKLOAD_PIN_NODES", 0),
		PodSecurityContext:            podSecurity,
		MountAgentPort:                config.GetIntEnv("KUBE_MOUNT_AGENT_PORT", 0),
		HostUsers:                     config.GetBoolEnv("KUBE_WORKLOAD_HOST_USERS", false),
		ClientQPS:                     config.GetIntEnv("KUBE_CLIENT_QPS", 0),
		ClientBurst:                   config.GetIntEnv("KUBE_CLIENT_BURST", 0),
		RequestTimeout:                config.GetDurationEnv("KUBE_REQUEST_TIMEOUT", 0),
		LeaderElection: LeaderElectionConfig{
			Enabled:       config.GetBoolEnv("KUBE_LEADER_ELECTION", false),
			LeaseName:     config.GetEnv("KUBE_LEADER_LEASE_NAME", ""),
			Identity:      config.GetEnv("KUBE_LEADER_IDENTITY", ""),
			LeaseDuration: config.GetDurationEnv("KUBE_LEADER_LEASE_DURATION", 0),
			RenewDeadline: config.GetDurationEnv("KUBE_LEADER_RENEW_DEADLINE", 0),
			RetryPeriod:   config.GetDurationEnv("KUBE_LEADER_RETRY_PERIOD", 0),
		},
	}
	cfg.LeaderElection.ApplyDefaults("jobs-service-leader")
	if cfg.RequestTimeout >= config.DefaultWriteTimeout {
		slog.Warn("KUBE_REQUEST_TIMEOUT is not under the API write timeout, so a stalled Kubernetes call outlives its response",
			"requestTimeout", cfg.RequestTimeout, "writeTimeout", config.DefaultWriteTimeout)
	}
	return cfg, nil
}
