package kubernetes

import (
	"orchestrator/internal/config"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	"orchestrator/internal/warm"
	"orchestrator/pkg/pool"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	defaultNamespace       = "orchestrator"
	defaultRunAsUser       = 65532 // distroless "nonroot"
	defaultGatewayName     = "orchestrator"
	defaultPoolDomain      = "localhost"
	defaultLeaderLeaseName = "deployments-service-pools-leader"

	defaultOrphanTTL = 60 * time.Second
)

// Config holds configuration for the Kubernetes pool orchestrator.
type Config struct {
	SidecarImage string      // workload-sidecar (proxy) image (set by the caller)
	ShimImage    string      // pool-shim image for the shim-install init container (set by the caller)
	Pools        []pool.Pool // configured pools (set by the caller from POOLS_JSON)

	Kubeconfig             string
	Context                string // kubeconfig context to pin; empty uses current-context
	Namespace              string
	SidecarImagePullPolicy string // applied to shim-install + proxy; empty = kubelet default
	WorkerImagePullPolicy  string // applied to the workload (pool image) container; empty = kubelet default
	RunAsUser              int64  // UID/GID for every container; default 65532

	// Overcommit derives warm-pod requests from declared limits (internal/kube).
	Overcommit kube.Overcommit
	// Tolerations are stamped on every warm pod (internal/kube).
	Tolerations []corev1.Toleration
	// NodeSelector pins every warm pod to a node pool (internal/kube).
	NodeSelector map[string]string

	// RuntimeClasses maps isolation tiers (gvisor, kata) to the
	// RuntimeClass stamped on warm pods (KUBE_RUNTIME_CLASSES,
	// "gvisor=gvisor,kata=kata-qemu"). runc never maps — it is the cluster
	// default. Defaults: gvisor→gvisor, kata→kata.
	RuntimeClasses map[string]string

	GatewayEnabled   bool   // reconcile per-activation HTTPRoutes; off for pre-Gateway clusters
	GatewayName      string // parentRef Gateway name
	GatewayNamespace string // parentRef Gateway namespace; default = Namespace

	PoolDomain string        // default hostname suffix for HTTP activations: {id}.{PoolDomain}
	OrphanTTL  time.Duration // discard claimed-but-unlabeled pods (crashed mid-claim) after this

	// LeaderElection gates the control loop (replenishment + GC) to one
	// replica; disabled = single-replica mode.
	LeaderElection kube.LeaderElectionConfig

	// Metrics receives K8s API, leadership, and pool telemetry. Set by the
	// caller (not the environment); may be nil in tests.
	Metrics *observability.Metrics
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables, mirroring the deployments Kubernetes backend's names where the
// concept matches. SidecarImage, ShimImage, and Pools are provided by the
// caller.
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
		Kubeconfig:             config.GetEnv("KUBECONFIG", ""),
		Context:                config.GetEnv("KUBE_CONTEXT", ""),
		Namespace:              config.GetEnv("KUBE_NAMESPACE", defaultNamespace),
		SidecarImagePullPolicy: config.GetEnv("KUBE_SIDECAR_IMAGE_PULL_POLICY", ""),
		WorkerImagePullPolicy:  config.GetEnv("KUBE_WORKER_IMAGE_PULL_POLICY", ""),
		RunAsUser:              int64(config.GetIntEnv("KUBE_RUN_AS_USER", defaultRunAsUser)),
		Overcommit:             kube.OvercommitFromEnv(),
		Tolerations:            tolerations,
		NodeSelector:           nodeSelector,
		RuntimeClasses:         classes,

		GatewayEnabled:   boolEnv("KUBE_GATEWAY_ENABLED", true),
		GatewayName:      config.GetEnv("KUBE_GATEWAY_NAME", defaultGatewayName),
		GatewayNamespace: config.GetEnv("KUBE_GATEWAY_NAMESPACE", ""),

		PoolDomain: config.GetEnv("POOL_DOMAIN", defaultPoolDomain),
		OrphanTTL:  config.GetDurationEnv("POOL_ORPHAN_TTL", defaultOrphanTTL),

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
	if c.PoolDomain == "" {
		c.PoolDomain = defaultPoolDomain
	}
	if c.OrphanTTL <= 0 {
		c.OrphanTTL = defaultOrphanTTL
	}
	if c.RuntimeClasses == nil {
		c.RuntimeClasses, _ = kube.ParseRuntimeClasses("")
	}
	if c.LeaderElection.Enabled {
		c.LeaderElection.ApplyDefaults(defaultLeaderLeaseName)
	}
}

// warmConfig projects the pool config onto the warm-pool manager's — the
// images, hardening, placement, and GC knobs it shares with every other warm
// consumer.
func (c *Config) warmConfig() warm.Config {
	return warm.Config{
		Namespace:              c.Namespace,
		SidecarImage:           c.SidecarImage,
		ShimImage:              c.ShimImage,
		SidecarImagePullPolicy: c.SidecarImagePullPolicy,
		WorkerImagePullPolicy:  c.WorkerImagePullPolicy,
		RunAsUser:              c.RunAsUser,
		Overcommit:             c.Overcommit,
		Tolerations:            c.Tolerations,
		NodeSelector:           c.NodeSelector,
		RuntimeClasses:         c.RuntimeClasses,
		OrphanTTL:              c.OrphanTTL,
		Naming:                 naming(),
		Metrics:                c.Metrics,
	}
}
