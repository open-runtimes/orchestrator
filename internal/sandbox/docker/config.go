package docker

import (
	"orchestrator/internal/config"
	"orchestrator/internal/pool"
	"orchestrator/internal/sandbox"
	"strings"
	"time"
)

const (
	defaultSandboxDomain = "localhost"

	// agentPath is where the agent-install container drops the sandbox agent,
	// and therefore the default command a sandbox runs (pkg/sandbox).
	agentPath = workspacePath + "/" + sandbox.AgentName

	// reapTick is how often the idle sweep runs. Docker has no leader election
	// and one service process, so the loop simply runs.
	reapTick = 2 * time.Second
)

// Config holds configuration for the Docker sandbox orchestrator.
type Config struct {
	SidecarImage    string // workload-sidecar (proxy) image (set by the caller)
	JobSidecarImage string // job-sidecar image for artifact materialization
	// AgentImage publishes the binary that serves the sandbox contract; it is
	// copied out of this image into each sandbox's workspace.
	AgentImage string
	Pools      []pool.Pool // configured sandbox pools (set by the caller from SANDBOX_POOLS_JSON)

	Network          string   // Docker network to attach sandbox containers to
	ArtifactEndpoint string   // base URL for sidecar artifact reporting
	ExtraHosts       []string // extra /etc/hosts entries for containers

	// SandboxDomain is the domain sandboxes are addressed under. On Docker the
	// in-process edge serves them on the deployments data port, so the URL
	// carries that port too.
	SandboxDomain string
	Scheme        string
	DataPort      string
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables. Images and Pools are provided by the caller.
func LoadConfigFromEnv() Config {
	var extraHosts []string
	if hosts := config.GetEnv("EXTRA_HOSTS", ""); hosts != "" {
		extraHosts = strings.Split(hosts, ",")
	}
	return Config{
		AgentImage:       config.GetEnv("SANDBOX_AGENT_IMAGE", sandbox.AgentImage),
		JobSidecarImage:  config.GetEnv("JOB_SIDECAR_IMAGE", "ghcr.io/open-runtimes/orchestrator/job-sidecar:latest"),
		Network:          config.GetEnv("DOCKER_NETWORK", ""),
		ArtifactEndpoint: config.GetEnv("ARTIFACT_ENDPOINT", "http://host.docker.internal:8080"),
		ExtraHosts:       extraHosts,
		SandboxDomain:    config.GetEnv("SANDBOX_DOMAIN", defaultSandboxDomain),
		Scheme:           config.GetEnv("SANDBOX_SCHEME", "http"),
		DataPort:         config.GetEnv("DATA_PORT", "8081"),
	}
}

func (c *Config) applyDefaults() {
	if c.AgentImage == "" {
		c.AgentImage = sandbox.AgentImage
	}
	if c.SandboxDomain == "" {
		c.SandboxDomain = defaultSandboxDomain
	}
	if c.Scheme == "" {
		c.Scheme = "http"
	}
}

// addressing is the sandbox host grammar (pkg/sandbox). Unlike Kubernetes,
// where a gateway fronts port 80, the Docker edge is the service's own data
// listener — so the port belongs in the URL.
func (c *Config) addressing() sandbox.Addressing {
	return sandbox.Addressing{Domain: c.SandboxDomain, Scheme: c.Scheme, Port: c.DataPort}
}
