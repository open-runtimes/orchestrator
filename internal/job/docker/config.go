package docker

import (
	"orchestrator/internal/config"
	"strings"
	"time"
)

// Config holds configuration for the Docker orchestrator.
type Config struct {
	SidecarImage        string
	JobRetention        time.Duration // How long to keep completed jobs
	MaintenanceInterval time.Duration // How often to run cleanup
	ArtifactEndpoint    string        // Base URL for sidecar artifact reporting (e.g., http://host.docker.internal:8080)
	ExtraHosts          []string      // Extra /etc/hosts entries for containers (e.g., ["appwrite.test:host-gateway"])
	Network             string        // Docker network to attach worker and sidecar containers to
}

// LoadConfigFromEnv loads orchestrator configuration from environment variables.
// SidecarImage is service-level configuration and is set by the caller.
func LoadConfigFromEnv() Config {
	var extraHosts []string
	if hosts := config.GetEnv("EXTRA_HOSTS", ""); hosts != "" {
		extraHosts = strings.Split(hosts, ",")
	}

	return Config{
		JobRetention:        config.GetDurationEnv("JOB_RETENTION", 15*time.Minute),
		MaintenanceInterval: config.GetDurationEnv("MAINTENANCE_INTERVAL", 1*time.Minute),
		ArtifactEndpoint:    config.GetEnv("ARTIFACT_ENDPOINT", "http://host.docker.internal:8080"),
		ExtraHosts:          extraHosts,
		Network:             config.GetEnv("ORCHESTRATOR_NETWORK", ""),
	}
}
