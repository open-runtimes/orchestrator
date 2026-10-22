// Package config provides configuration loading from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ServiceConfig holds configuration shared by the orchestrator services.
// Image names are namespace-prefixed (job/deployment) because this config is
// read by more than one binary — nothing here may assume the jobs context.
type ServiceConfig struct {
	Port              string
	APIKey            string
	ShutdownDrainWait time.Duration // Time to wait for load balancer to drain (0 to skip)
	JobSidecarImage   string        // job-sidecar image (job pods + deployments' artifact-pre)
}

// EnvAllowUnauthenticated is the explicit opt-in for running the Docker
// backend without an API key, for a local development profile.
const EnvAllowUnauthenticated = "ALLOW_UNAUTHENTICATED"

// LoadServiceConfig loads service configuration from environment variables.
//
// The API key comes from API_KEY_FILE, or from API_KEY when no file is named.
// A named key file that cannot be read, or that holds no key, is an error: the
// operator asked for authentication, and serving the API open instead is the
// one outcome that must not happen.
func LoadServiceConfig() (*ServiceConfig, error) {
	apiKey, err := loadAPIKey()
	if err != nil {
		return nil, err
	}
	return &ServiceConfig{
		Port:              GetEnv("PORT", "8080"),
		APIKey:            apiKey,
		ShutdownDrainWait: GetDurationEnv("SHUTDOWN_DRAIN_WAIT", 5*time.Second),
		JobSidecarImage:   GetEnv("JOB_SIDECAR_IMAGE", "job-sidecar:latest"),
	}, nil
}

func loadAPIKey() (string, error) {
	path := GetEnv("API_KEY_FILE", "")
	if path == "" {
		return strings.TrimSpace(os.Getenv("API_KEY")), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read API_KEY_FILE: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("API_KEY_FILE %s holds no key", path)
	}
	return key, nil
}

// RequireAPIKey refuses to serve the Docker backend without an API key unless
// ALLOW_UNAUTHENTICATED=true opts in. Its API starts containers on the host's
// daemon with the image, command and volumes a request names, so an open API
// is the host. Other backends keep their own policy (the Helm chart's auth
// values) and pass.
func (c *ServiceConfig) RequireAPIKey(backend string) error {
	if c.APIKey != "" || backend != "docker" || GetEnv(EnvAllowUnauthenticated, "") == "true" {
		return nil
	}
	return errors.New("the Docker backend needs an API key: set API_KEY_FILE or API_KEY (or " + EnvAllowUnauthenticated + "=true for a local, unauthenticated profile)")
}
