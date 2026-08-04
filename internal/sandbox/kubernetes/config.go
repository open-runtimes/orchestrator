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
	defaultSandboxDomain   = "localhost"
	defaultLeaderLeaseName = "deployments-service-sandboxes-leader"

	defaultOrphanTTL = 60 * time.Second

	// hostPrefix leads every sandbox hostname, so the wildcard route and the
	// token are never confused for each other: {hostPrefix}{token}.{domain}.
	hostPrefix = "s-"
)

// Config holds configuration for the Kubernetes sandbox orchestrator.
type Config struct {
	SidecarImage string      // deployments-sidecar (proxy) image (set by the caller)
	ShimImage    string      // pool-shim image for the shim-install init container (set by the caller)
	Pools        []pool.Pool // configured sandbox pools (set by the caller from SANDBOX_POOLS_JSON)

	Kubeconfig             string
	Context                string // kubeconfig context to pin; empty uses current-context
	Namespace              string
	SidecarImagePullPolicy string
	WorkerImagePullPolicy  string
	RunAsUser              int64

	// Overcommit derives warm-pod requests from declared limits (internal/kube).
	Overcommit kube.Overcommit
	// Tolerations are stamped on every warm pod (internal/kube).
	Tolerations []corev1.Toleration
	// NodeSelector pins every warm pod to a node pool (internal/kube).
	NodeSelector map[string]string
	// RuntimeClasses maps isolation tiers (gvisor, kata) to the RuntimeClass
	// stamped on warm pods (KUBE_RUNTIME_CLASSES). Untrusted, model-generated
	// code is the expected sandbox workload, so gvisor or kata is the right
	// choice for a sandbox pool even though runc is the platform default.
	RuntimeClasses map[string]string

	// SandboxDomain is the wildcard domain sandboxes are reached at:
	// s-{token}.{SandboxDomain}. One HTTPRoute for *.{SandboxDomain} backs it
	// (operator config, in the chart) — there is no per-sandbox route to
	// program, so a create is as fast as the claim.
	SandboxDomain string
	// Scheme is the URL scheme handed back to callers (https where the gateway
	// terminates TLS).
	Scheme string

	OrphanTTL time.Duration // discard claimed-but-unlabeled pods (crashed mid-claim) after this

	// LeaderElection gates the control loop (replenishment + GC) to one
	// replica; disabled = single-replica mode.
	LeaderElection kube.LeaderElectionConfig

	// Metrics receives K8s API, leadership, and warm-pool telemetry. Set by
	// the caller (not the environment); may be nil in tests.
	Metrics *observability.Metrics
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables. SidecarImage, ShimImage, and Pools are provided by the caller.
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

		SandboxDomain: config.GetEnv("SANDBOX_DOMAIN", defaultSandboxDomain),
		Scheme:        config.GetEnv("SANDBOX_SCHEME", "http"),
		OrphanTTL:     config.GetDurationEnv("SANDBOX_ORPHAN_TTL", defaultOrphanTTL),

		LeaderElection: kube.LeaderElectionConfig{
			Enabled:       config.GetEnv("KUBE_LEADER_ELECTION", "") == "true",
			LeaseName:     config.GetEnv("KUBE_SANDBOX_LEADER_LEASE_NAME", defaultLeaderLeaseName),
			Identity:      config.GetEnv("KUBE_LEADER_IDENTITY", ""),
			LeaseDuration: config.GetDurationEnv("KUBE_LEADER_LEASE_DURATION", 15*time.Second),
			RenewDeadline: config.GetDurationEnv("KUBE_LEADER_RENEW_DEADLINE", 10*time.Second),
			RetryPeriod:   config.GetDurationEnv("KUBE_LEADER_RETRY_PERIOD", 2*time.Second),
		},
	}, nil
}

func (c *Config) applyDefaults() {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.RunAsUser <= 0 {
		c.RunAsUser = defaultRunAsUser
	}
	if c.SandboxDomain == "" {
		c.SandboxDomain = defaultSandboxDomain
	}
	if c.Scheme == "" {
		c.Scheme = "http"
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

// URLFor builds a sandbox's URL from its capability token. The token — not the
// caller-chosen id — is the address, so a guessed id reaches nothing.
func (c *Config) URLFor(token string) string {
	if token == "" {
		return ""
	}
	return c.Scheme + "://" + hostPrefix + token + "." + c.SandboxDomain
}

// PortURLFor addresses one of a sandbox's secondary ports. The port rides in
// the SAME DNS label as the token rather than a label of its own: a wildcard
// certificate covers exactly one label (RFC 6125), so s-{token}-{port} is
// reachable under one *.{domain} cert while s-{port}.{token}.{domain} would
// need a certificate per sandbox.
func (c *Config) PortURLFor(token string, port int) string {
	if token == "" {
		return ""
	}
	return c.Scheme + "://" + hostPrefix + token + "-" + strconv.Itoa(port) + "." + c.SandboxDomain
}

// URLsFor addresses every port a sandbox serves, keyed by port number: the
// pool's own primary plus each extra port the request declared.
func (c *Config) URLsFor(token string, primary int, ports []int) map[string]string {
	if token == "" {
		return nil
	}
	urls := make(map[string]string, len(ports)+1)
	urls[strconv.Itoa(primary)] = c.URLFor(token)
	for _, port := range ports {
		urls[strconv.Itoa(port)] = c.PortURLFor(token, port)
	}
	return urls
}

// warmConfig projects the sandbox config onto the warm-pool manager's.
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
