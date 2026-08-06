package kubernetes

import (
	"orchestrator/internal/config"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	"orchestrator/internal/warm"
	"orchestrator/pkg/pool"
	"orchestrator/pkg/sandbox"
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

	// workspacePath mirrors internal/warm's workspace mount.
	workspacePath = config.DefaultWorkspace
	// agentPath is where the agent-install container drops the sandbox agent,
	// and therefore the default command a sandbox execs (pkg/sandbox).
	agentPath = workspacePath + "/" + sandbox.AgentName
)

// Config holds configuration for the Kubernetes sandbox orchestrator.
type Config struct {
	SidecarImage string // workload-sidecar (proxy) image (set by the caller)
	ShimImage    string // pool-shim image for the shim-install init container (set by the caller)
	// AgentImage publishes the binary that serves the sandbox contract. It is
	// copied out of this image into every warm pod's workspace, which is what
	// lets a pool run an ordinary runtime image.
	AgentImage string
	Pools      []pool.Pool // configured sandbox pools (set by the caller from SANDBOX_POOLS_JSON)

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

		AgentImage:    config.GetEnv("SANDBOX_AGENT_IMAGE", sandbox.AgentImage),
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
	if c.AgentImage == "" {
		c.AgentImage = sandbox.AgentImage
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

// addressing is the sandbox host grammar (pkg/sandbox), which both renders
// these URLs and is what the sandbox proxy reads them back with.
func (c *Config) addressing() sandbox.Addressing {
	return sandbox.Addressing{Domain: c.SandboxDomain, Scheme: c.Scheme}
}

// AgentCommand is the command a sandbox runs unless the pool or the request
// names another: the agent the shim installs into the workspace, which serves
// the sandbox contract on behalf of ANY image.
func AgentCommand() string { return agentPath }

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
		LeaderElection:         c.LeaderElection,

		// Every sandbox pool gets the agent, so a pool's image needs to serve
		// nothing itself; the agent is told which port to listen on and where the
		// workspace is.
		Agent: warm.Agent{Image: c.AgentImage, Source: sandbox.AgentSource, Dest: agentPath},
		WorkloadEnv: func(p *pool.Pool) map[string]string {
			return map[string]string{
				"SANDBOX_PORT":      strconv.Itoa(p.Port),
				"SANDBOX_WORKSPACE": workspacePath,
			}
		},
	}
}
