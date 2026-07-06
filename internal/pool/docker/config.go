package docker

import (
	"orchestrator/internal/config"
	"orchestrator/internal/observability"
	"orchestrator/pkg/pool"
	"strings"
	"time"
)

// Config holds configuration for the Docker pool orchestrator.
type Config struct {
	SidecarImage string      // deployments-sidecar image (set by the caller, e.g. from DEPLOYMENT_SIDECAR_IMAGE)
	ShimImage    string      // pool-shim image for the shim-install step (set by the caller)
	Pools        []pool.Pool // configured pools (set by the caller from POOLS_JSON)
	Network      string      // Docker network to attach slot containers to
	ExtraHosts   []string    // Extra /etc/hosts entries for the sidecar (e.g., ["appwrite.test:host-gateway"])
	Retention    time.Duration

	// Metrics receives pool telemetry. Set by the caller; may be nil in tests.
	Metrics *observability.Metrics
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables. SidecarImage, ShimImage, and Pools are provided by the caller.
func LoadConfigFromEnv() Config {
	var extraHosts []string
	if hosts := config.GetEnv("EXTRA_HOSTS", ""); hosts != "" {
		extraHosts = strings.Split(hosts, ",")
	}

	return Config{
		Network:    config.GetEnv("DOCKER_NETWORK", ""),
		ExtraHosts: extraHosts,
		Retention:  config.GetDurationEnv("POOL_ACTIVATION_RETENTION", 15*time.Minute),
	}
}
