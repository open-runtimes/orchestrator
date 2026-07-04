package kubernetes

import "orchestrator/internal/config"

const (
	defaultNamespace = "orchestrator"
	defaultRunAsUser = 65532 // distroless "nonroot"
)

// Config holds configuration for the Kubernetes deployment orchestrator.
type Config struct {
	SidecarImage           string // deployments-sidecar (proxy) image (set by the caller, e.g. from SIDECAR_IMAGE)
	ArtifactImage          string // job-sidecar image for the artifact-pre init container
	Kubeconfig             string
	Context                string // kubeconfig context to pin; empty uses current-context
	Namespace              string
	ServiceAccount         string // pod ServiceAccount; empty uses the namespace default
	SidecarImagePullPolicy string // applied to artifact-pre + proxy; empty = kubelet default
	WorkerImagePullPolicy  string // applied to the worker (user) container; empty = kubelet default
	RunAsUser              int64  // UID/GID for every container; default 65532
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables, mirroring the jobs Kubernetes backend's names where the concept
// matches. SidecarImage is provided by the caller.
func LoadConfigFromEnv() Config {
	return Config{
		ArtifactImage:          config.GetEnv("ARTIFACT_IMAGE", "ghcr.io/open-runtimes/orchestrator/job-sidecar:latest"),
		Kubeconfig:             config.GetEnv("KUBECONFIG", ""),
		Context:                config.GetEnv("KUBE_CONTEXT", ""),
		Namespace:              config.GetEnv("KUBE_NAMESPACE", defaultNamespace),
		ServiceAccount:         config.GetEnv("KUBE_DEPLOYMENT_SERVICE_ACCOUNT", ""),
		SidecarImagePullPolicy: config.GetEnv("KUBE_SIDECAR_IMAGE_PULL_POLICY", ""),
		WorkerImagePullPolicy:  config.GetEnv("KUBE_WORKER_IMAGE_PULL_POLICY", ""),
		RunAsUser:              int64(config.GetIntEnv("KUBE_RUN_AS_USER", defaultRunAsUser)),
	}
}

func (c *Config) applyDefaults() {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.RunAsUser <= 0 {
		c.RunAsUser = defaultRunAsUser
	}
}
