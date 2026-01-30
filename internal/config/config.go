// Package config provides configuration loading from environment variables.
package config

import (
	"time"
)

// ServiceConfig holds configuration for the jobs service.
type ServiceConfig struct {
	Port              string
	MetricsPort       string
	APIKey            string
	ShutdownDrainWait time.Duration // Time to wait for load balancer to drain (0 to skip)
	SidecarImage      string        // Sidecar image for all orchestrators
}

// LoadServiceConfig loads service configuration from environment variables.
func LoadServiceConfig() *ServiceConfig {
	return &ServiceConfig{
		Port:              GetEnv("PORT", "8080"),
		MetricsPort:       GetEnv("METRICS_PORT", "9090"),
		APIKey:            GetSecretFile(GetEnv("API_KEY_FILE", "")),
		ShutdownDrainWait: GetDurationEnv("SHUTDOWN_DRAIN_WAIT", 5*time.Second),
		SidecarImage:      GetEnv("SIDECAR_IMAGE", "job-sidecar:latest"),
	}
}
