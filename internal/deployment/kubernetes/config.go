package kubernetes

import (
	"orchestrator/internal/config"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	defaultNamespace            = "orchestrator"
	defaultRunAsUser            = 65532 // distroless "nonroot"
	defaultGatewayName          = "orchestrator"
	defaultActivatorService     = "deployments-activator"
	defaultActivatorPort        = 8081
	defaultActivatorSelector    = "app.kubernetes.io/component=deployments-activator"
	defaultRevisionHistoryLimit = 3
	defaultLeaderLeaseName      = "deployments-service-leader"
)

// Config holds configuration for the Kubernetes deployment orchestrator.
type Config struct {
	SidecarImage           string // deployments-sidecar (proxy) image (set by the caller, e.g. from DEPLOYMENT_SIDECAR_IMAGE)
	JobSidecarImage        string // job-sidecar image for the artifact-pre init container
	Kubeconfig             string
	Context                string // kubeconfig context to pin; empty uses current-context
	Namespace              string
	ServiceAccount         string // pod ServiceAccount; empty uses the namespace default
	SidecarImagePullPolicy string // applied to artifact-pre + proxy; empty = kubelet default
	WorkerImagePullPolicy  string // applied to the worker (user) container; empty = kubelet default
	RunAsUser              int64  // UID/GID for every container; default 65532

	// Overcommit derives worker requests from declared limits (internal/kube).
	Overcommit kube.Overcommit
	// Tolerations are stamped on every workload pod (internal/kube).
	Tolerations []corev1.Toleration
	// NodeSelector pins every workload pod to a node pool (internal/kube).
	NodeSelector map[string]string

	// RuntimeClasses maps isolation tiers (gvisor, kata) to the
	// RuntimeClass stamped on workload pods (KUBE_RUNTIME_CLASSES,
	// "gvisor=gvisor,kata=kata-qemu"). runc never maps — it is the cluster
	// default. Defaults: gvisor→gvisor, kata→kata.
	RuntimeClasses map[string]string

	GatewayEnabled       bool   // reconcile HTTPRoutes + the cold endpoint flip; off for pre-Gateway clusters
	GatewayName          string // parentRef Gateway name
	GatewayNamespace     string // parentRef Gateway namespace; default = Namespace
	ActivatorService     string // Service backing the Prefer: respond-async rule
	ActivatorNamespace   string // namespace of the activator Service and pods; empty = Namespace
	ActivatorPort        int    // activator listen port
	ActivatorSelector    string // pod selector for activator endpoints (cold endpoint flip)
	RevisionHistoryLimit int    // retained revisions beyond the routed set; default 3

	// LeaderElection gates the background reconcilers (and any caller-supplied
	// loop via RunLeaderElected) to one replica; disabled = single-replica mode.
	LeaderElection kube.LeaderElectionConfig

	// Metrics receives K8s API, leadership, and rollout telemetry. Set by the
	// caller (not the environment); may be nil in tests.
	Metrics *observability.Metrics
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables, mirroring the jobs Kubernetes backend's names where the concept
// matches. SidecarImage is provided by the caller.
func LoadConfigFromEnv() (Config, error) {
	classes, err := kube.ParseRuntimeClasses(config.GetEnv("KUBE_RUNTIME_CLASSES", ""))
	if err != nil {
		return Config{}, err
	}
	tolerations, err := kube.TolerationsFromEnv()
	if err != nil {
		return Config{}, err
	}
	nodeSelector, err := kube.NodeSelectorFromEnv()
	if err != nil {
		return Config{}, err
	}
	return Config{
		JobSidecarImage:        config.GetEnv("JOB_SIDECAR_IMAGE", "ghcr.io/open-runtimes/orchestrator/job-sidecar:latest"),
		Kubeconfig:             config.GetEnv("KUBECONFIG", ""),
		Context:                config.GetEnv("KUBE_CONTEXT", ""),
		Namespace:              config.GetEnv("KUBE_NAMESPACE", defaultNamespace),
		ServiceAccount:         config.GetEnv("KUBE_DEPLOYMENT_SERVICE_ACCOUNT", ""),
		SidecarImagePullPolicy: config.GetEnv("KUBE_SIDECAR_IMAGE_PULL_POLICY", ""),
		WorkerImagePullPolicy:  config.GetEnv("KUBE_WORKER_IMAGE_PULL_POLICY", ""),
		RunAsUser:              int64(config.GetIntEnv("KUBE_RUN_AS_USER", defaultRunAsUser)),
		Overcommit:             kube.OvercommitFromEnv(),
		Tolerations:            tolerations,
		NodeSelector:           nodeSelector,
		RuntimeClasses:         classes,

		GatewayEnabled:       boolEnv("KUBE_GATEWAY_ENABLED", true),
		GatewayName:          config.GetEnv("KUBE_GATEWAY_NAME", defaultGatewayName),
		GatewayNamespace:     config.GetEnv("KUBE_GATEWAY_NAMESPACE", ""),
		ActivatorService:     config.GetEnv("ACTIVATOR_SERVICE", defaultActivatorService),
		ActivatorNamespace:   config.GetEnv("KUBE_ACTIVATOR_NAMESPACE", ""),
		ActivatorPort:        config.GetIntEnv("ACTIVATOR_PORT", defaultActivatorPort),
		ActivatorSelector:    config.GetEnv("ACTIVATOR_SELECTOR", defaultActivatorSelector),
		RevisionHistoryLimit: config.GetIntEnv("REVISION_HISTORY_LIMIT", defaultRevisionHistoryLimit),

		LeaderElection: kube.LeaderElectionConfig{
			Enabled:       config.GetEnv("KUBE_LEADER_ELECTION", "") == "true",
			LeaseName:     config.GetEnv("KUBE_LEADER_LEASE_NAME", defaultLeaderLeaseName),
			Identity:      config.GetEnv("KUBE_LEADER_IDENTITY", ""),
			LeaseDuration: config.GetDurationEnv("KUBE_LEADER_LEASE_DURATION", 15*time.Second),
			RenewDeadline: config.GetDurationEnv("KUBE_LEADER_RENEW_DEADLINE", 10*time.Second),
			RetryPeriod:   config.GetDurationEnv("KUBE_LEADER_RETRY_PERIOD", 2*time.Second),
		},
	}, nil
}

// boolEnv parses a boolean environment variable, falling back to the default
// on absence or a malformed value.
func boolEnv(key string, defaultValue bool) bool {
	if v, err := strconv.ParseBool(config.GetEnv(key, strconv.FormatBool(defaultValue))); err == nil {
		return v
	}
	return defaultValue
}

func (c *Config) applyDefaults() {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.RunAsUser <= 0 {
		c.RunAsUser = defaultRunAsUser
	}
	if c.GatewayName == "" {
		c.GatewayName = defaultGatewayName
	}
	if c.GatewayNamespace == "" {
		c.GatewayNamespace = c.Namespace
	}
	if c.ActivatorService == "" {
		c.ActivatorService = defaultActivatorService
	}
	if c.ActivatorPort <= 0 {
		c.ActivatorPort = defaultActivatorPort
	}
	if c.ActivatorSelector == "" {
		c.ActivatorSelector = defaultActivatorSelector
	}
	if c.RevisionHistoryLimit <= 0 {
		c.RevisionHistoryLimit = defaultRevisionHistoryLimit
	}
	if c.RuntimeClasses == nil {
		c.RuntimeClasses, _ = kube.ParseRuntimeClasses("")
	}
	if c.LeaderElection.Enabled {
		c.LeaderElection.ApplyDefaults(defaultLeaderLeaseName)
	}
}
