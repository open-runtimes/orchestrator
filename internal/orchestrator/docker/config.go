package docker

import (
	"orchestrator/internal/config"
	"strings"
	"time"
)

// OrchestratorConfig holds configuration for the container orchestrator.
type OrchestratorConfig struct {
	JobRetention        time.Duration // How long to keep completed jobs
	MaintenanceInterval time.Duration // How often to run cleanup
	CallbackProxyURL    string        // Internal URL for sidecar callback proxy (e.g., http://host.docker.internal:8080)
	ExtraHosts          []string      // Extra hosts for containers (e.g., ["appwrite.test:host-gateway"])
}

// LoadConfigFromEnv loads orchestrator configuration from environment variables.
func LoadConfigFromEnv() OrchestratorConfig {
	var extraHosts []string
	if hosts := config.GetEnv("EXTRA_HOSTS", ""); hosts != "" {
		extraHosts = strings.Split(hosts, ",")
	}

	return OrchestratorConfig{
		JobRetention:        config.GetDurationEnv("JOB_RETENTION", 15*time.Minute),
		MaintenanceInterval: config.GetDurationEnv("MAINTENANCE_INTERVAL", 1*time.Minute),
		CallbackProxyURL:    config.GetEnv("CALLBACK_PROXY_URL", "http://host.docker.internal:8080"),
		ExtraHosts:          extraHosts,
	}
}
