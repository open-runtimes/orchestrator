package sidecar

import (
	"orchestrator/internal/config"
	"time"
)

// Config holds configuration for the job sidecar.
type Config struct {
	JobID            string
	ArtifactEndpoint string        // Base URL of the orchestrator (e.g., http://host.docker.internal:8080)
	ArtifactToken    string        // Per-job bearer token for artifact reporting
	ArtifactTimeout  time.Duration // Per-request timeout for artifact reporting
	TimeoutSeconds   int
	SharedVolumePath string
	Meta             string
	CallbackURL      string
	CallbackKey      string
	CallbackEvents   string // comma-separated event type filter
	S3               config.S3Credentials
}

// LoadConfigFromEnv loads sidecar configuration from environment variables.
func LoadConfigFromEnv() *Config {
	return &Config{
		JobID:            config.GetEnv("JOB_ID", ""),
		ArtifactEndpoint: config.GetEnv("ARTIFACT_ENDPOINT", ""),
		ArtifactToken:    config.GetEnv("ARTIFACT_TOKEN", ""),
		ArtifactTimeout:  config.GetDurationEnv("ARTIFACT_TIMEOUT", 30*time.Second),
		TimeoutSeconds:   config.GetIntEnv("TIMEOUT_SECONDS", 1800),
		SharedVolumePath: config.GetEnv("SHARED_VOLUME_PATH", "/workspace"),
		Meta:             config.GetEnv("JOB_META", "{}"),
		CallbackURL:      config.GetEnv("CALLBACK_URL", ""),
		CallbackKey:      config.GetEnv("CALLBACK_KEY", ""),
		CallbackEvents:   config.GetEnv("CALLBACK_EVENTS", ""),
		S3:               config.LoadS3Credentials(),
	}
}
