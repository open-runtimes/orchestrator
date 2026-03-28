package sidecar

import (
	"orchestrator/internal/config"
	"time"
)

// Config holds configuration for the job sidecar.
type Config struct {
	JobID            string
	ArtifactsJSON    string
	ArtifactEndpoint string        // Base URL of the orchestrator (e.g., http://host.docker.internal:8080)
	ArtifactTimeout  time.Duration // Per-request timeout for artifact reporting
	TimeoutSeconds   int
	SharedVolumePath string
	Meta             string
	CallbackURL      string
	CallbackKey      string
	CallbackEvents   string // comma-separated event type filter
}

// LoadConfigFromEnv loads sidecar configuration from environment variables.
func LoadConfigFromEnv() *Config {
	return &Config{
		JobID:            config.GetEnv("JOB_ID", ""),
		ArtifactsJSON:    config.GetEnv("ARTIFACTS_JSON", "[]"),
		ArtifactEndpoint: config.GetEnv("ARTIFACT_ENDPOINT", ""),
		ArtifactTimeout:  config.GetDurationEnv("ARTIFACT_TIMEOUT", 30*time.Second),
		TimeoutSeconds:   config.GetIntEnv("TIMEOUT_SECONDS", 1800),
		SharedVolumePath: config.GetEnv("SHARED_VOLUME_PATH", "/workspace"),
		Meta:             config.GetEnv("JOB_META", "{}"),
		CallbackURL:      config.GetEnv("CALLBACK_URL", ""),
		CallbackKey:      config.GetEnv("CALLBACK_KEY", ""),
		CallbackEvents:   config.GetEnv("CALLBACK_EVENTS", ""),
	}
}
