// Package config provides configuration loading from environment variables.
package config

import (
	"time"
)

// ServiceConfig holds configuration shared by the orchestrator services.
// Image names are namespace-prefixed (job/deployment) because this config is
// read by more than one binary — nothing here may assume the jobs context.
type ServiceConfig struct {
	Port              string
	MetricsPort       string
	APIKey            string
	ShutdownDrainWait time.Duration // Time to wait for load balancer to drain (0 to skip)
	JobSidecarImage   string        // job-sidecar image (job pods + deployments' artifact-pre)
}

// LoadServiceConfig loads service configuration from environment variables.
func LoadServiceConfig() *ServiceConfig {
	return &ServiceConfig{
		Port:              GetEnv("PORT", "8080"),
		MetricsPort:       GetEnv("METRICS_PORT", "9090"),
		APIKey:            GetSecretFile(GetEnv("API_KEY_FILE", "")),
		ShutdownDrainWait: GetDurationEnv("SHUTDOWN_DRAIN_WAIT", 5*time.Second),
		JobSidecarImage:   GetEnv("JOB_SIDECAR_IMAGE", "job-sidecar:latest"),
	}
}
