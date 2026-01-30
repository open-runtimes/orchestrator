package sidecar

import (
	"orchestrator/internal/config"
	"time"
)

// Config holds configuration for the job sidecar.
type Config struct {
	JobID            string
	ArtifactsJSON    string
	CallbackURL      string
	CallbackEvents   string
	CallbackKey      string
	CallbackTimeout  time.Duration
	UploadTimeout    time.Duration
	UploadRetries    int
	TimeoutSeconds   int
	SharedVolumePath string
	Meta             string
}

// LoadConfigFromEnv loads sidecar configuration from environment variables.
func LoadConfigFromEnv() *Config {
	return &Config{
		JobID:            config.GetEnv("JOB_ID", ""),
		ArtifactsJSON:    config.GetEnv("ARTIFACTS_JSON", "[]"),
		CallbackURL:      config.GetEnv("CALLBACK_URL", ""),
		CallbackEvents:   config.GetEnv("CALLBACK_EVENTS", ""),
		CallbackKey:      config.GetEnv("CALLBACK_KEY", ""),
		CallbackTimeout:  config.GetDurationEnv("CALLBACK_TIMEOUT", 30*time.Second),
		UploadTimeout:    config.GetDurationEnv("UPLOAD_TIMEOUT", 5*time.Minute),
		UploadRetries:    config.GetIntEnv("UPLOAD_RETRIES", 3),
		TimeoutSeconds:   config.GetIntEnv("TIMEOUT_SECONDS", 1800),
		SharedVolumePath: config.GetEnv("SHARED_VOLUME_PATH", "/workspace"),
		Meta:             config.GetEnv("JOB_META", "{}"),
	}
}
