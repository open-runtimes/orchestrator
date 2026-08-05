package docker

import (
	"orchestrator/internal/config"
	"orchestrator/pkg/pool"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSandboxDomain = "localhost"
	// hostPrefix leads every sandbox hostname, matching the Kubernetes backend:
	// {hostPrefix}{token}.{domain}, and {hostPrefix}{token}-{port}.{domain}.
	hostPrefix = "s-"

	// reapTick is how often the idle sweep runs. Docker has no leader election
	// and one service process, so the loop simply runs.
	reapTick = 2 * time.Second
)

// Config holds configuration for the Docker sandbox orchestrator.
type Config struct {
	SidecarImage    string      // deployments-sidecar (proxy) image (set by the caller)
	JobSidecarImage string      // job-sidecar image for artifact materialization
	Pools           []pool.Pool // configured sandbox pools (set by the caller from SANDBOX_POOLS_JSON)

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
	if c.SandboxDomain == "" {
		c.SandboxDomain = defaultSandboxDomain
	}
	if c.Scheme == "" {
		c.Scheme = "http"
	}
}

// URLFor builds a sandbox's URL from its capability token. Unlike Kubernetes,
// where a gateway fronts port 80, the Docker edge is the service's own data
// listener — so the port belongs in the URL.
func (c *Config) URLFor(token string) string {
	if token == "" {
		return ""
	}
	return c.Scheme + "://" + hostPrefix + token + "." + c.SandboxDomain + c.portSuffix()
}

// PortURLFor addresses one of a sandbox's extra ports. The port shares the
// token's DNS label, as on Kubernetes, so the two backends hand out the same
// hostname shape.
func (c *Config) PortURLFor(token string, port int) string {
	if token == "" {
		return ""
	}
	return c.Scheme + "://" + hostPrefix + token + "-" + strconv.Itoa(port) + "." + c.SandboxDomain + c.portSuffix()
}

// URLsFor addresses every port a sandbox serves, keyed by port number.
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

func (c *Config) portSuffix() string {
	if c.DataPort == "" || c.DataPort == "80" {
		return ""
	}
	return ":" + c.DataPort
}
