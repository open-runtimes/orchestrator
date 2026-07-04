package docker

import (
	"orchestrator/internal/config"
	"strings"
)

// Config holds configuration for the Docker deployment orchestrator.
type Config struct {
	SidecarImage     string   // deployments-sidecar (proxy) image (set by the caller, e.g. from SIDECAR_IMAGE)
	ArtifactImage    string   // job-sidecar image for artifact materialization
	Network          string   // Docker network to attach deployment containers to
	ArtifactEndpoint string   // Base URL for sidecar artifact reporting (e.g., http://host.docker.internal:8080)
	ExtraHosts       []string // Extra /etc/hosts entries for containers (e.g., ["appwrite.test:host-gateway"])
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables. SidecarImage is provided by the caller.
func LoadConfigFromEnv() Config {
	var extraHosts []string
	if hosts := config.GetEnv("EXTRA_HOSTS", ""); hosts != "" {
		extraHosts = strings.Split(hosts, ",")
	}

	return Config{
		ArtifactImage:    config.GetEnv("ARTIFACT_IMAGE", "ghcr.io/open-runtimes/orchestrator/job-sidecar:latest"),
		Network:          config.GetEnv("DOCKER_NETWORK", ""),
		ArtifactEndpoint: config.GetEnv("ARTIFACT_ENDPOINT", "http://host.docker.internal:8080"),
		ExtraHosts:       extraHosts,
	}
}
