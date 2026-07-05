package kubernetes

import (
	"orchestrator/internal/config"
	"strconv"
)

const (
	defaultNamespace            = "orchestrator"
	defaultRunAsUser            = 65532 // distroless "nonroot"
	defaultGatewayName          = "orchestrator"
	defaultActivatorService     = "deployments-activator"
	defaultActivatorPort        = 8081
	defaultActivatorSelector    = "app.kubernetes.io/component=deployments-activator"
	defaultRevisionHistoryLimit = 3
)

// Config holds configuration for the Kubernetes deployment orchestrator.
type Config struct {
	SidecarImage           string // deployments-sidecar (proxy) image (set by the caller, e.g. from DEPLOYMENT_SIDECAR_IMAGE)
	JobSidecarImage        string // job-sidecar image for the artifact-pre init container
	Kubeconfig             string
	Context                string // kubeconfig context to pin; empty uses current-context
	Namespace              string
	ServiceAccount         string // pod ServiceAccount; empty uses the namespace default
	SidecarImagePullPolicy string // applied to artifact-pre + proxy; empty = kubelet default
	WorkerImagePullPolicy  string // applied to the worker (user) container; empty = kubelet default
	RunAsUser              int64  // UID/GID for every container; default 65532

	GatewayEnabled       bool   // reconcile HTTPRoutes + the cold endpoint flip; off for pre-Gateway clusters
	GatewayName          string // parentRef Gateway name
	GatewayNamespace     string // parentRef Gateway namespace; default = Namespace
	ActivatorService     string // Service backing the Prefer: respond-async rule
	ActivatorPort        int    // activator listen port
	ActivatorSelector    string // pod selector for activator endpoints (cold endpoint flip)
	RevisionHistoryLimit int    // retained revisions beyond the routed set; default 3
}

// LoadConfigFromEnv loads orchestrator configuration from environment
// variables, mirroring the jobs Kubernetes backend's names where the concept
// matches. SidecarImage is provided by the caller.
func LoadConfigFromEnv() Config {
	return Config{
		JobSidecarImage:        config.GetEnv("JOB_SIDECAR_IMAGE", "ghcr.io/open-runtimes/orchestrator/job-sidecar:latest"),
		Kubeconfig:             config.GetEnv("KUBECONFIG", ""),
		Context:                config.GetEnv("KUBE_CONTEXT", ""),
		Namespace:              config.GetEnv("KUBE_NAMESPACE", defaultNamespace),
		ServiceAccount:         config.GetEnv("KUBE_DEPLOYMENT_SERVICE_ACCOUNT", ""),
		SidecarImagePullPolicy: config.GetEnv("KUBE_SIDECAR_IMAGE_PULL_POLICY", ""),
		WorkerImagePullPolicy:  config.GetEnv("KUBE_WORKER_IMAGE_PULL_POLICY", ""),
		RunAsUser:              int64(config.GetIntEnv("KUBE_RUN_AS_USER", defaultRunAsUser)),

		GatewayEnabled:       boolEnv("KUBE_GATEWAY_ENABLED", true),
		GatewayName:          config.GetEnv("KUBE_GATEWAY_NAME", defaultGatewayName),
		GatewayNamespace:     config.GetEnv("KUBE_GATEWAY_NAMESPACE", ""),
		ActivatorService:     config.GetEnv("ACTIVATOR_SERVICE", defaultActivatorService),
		ActivatorPort:        config.GetIntEnv("ACTIVATOR_PORT", defaultActivatorPort),
		ActivatorSelector:    config.GetEnv("ACTIVATOR_SELECTOR", defaultActivatorSelector),
		RevisionHistoryLimit: config.GetIntEnv("REVISION_HISTORY_LIMIT", defaultRevisionHistoryLimit),
	}
}

// boolEnv parses a boolean environment variable, falling back to the default
// on absence or a malformed value.
func boolEnv(key string, defaultValue bool) bool {
	if v, err := strconv.ParseBool(config.GetEnv(key, strconv.FormatBool(defaultValue))); err == nil {
		return v
	}
	return defaultValue
}

func (c *Config) applyDefaults() {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.RunAsUser <= 0 {
		c.RunAsUser = defaultRunAsUser
	}
	if c.GatewayName == "" {
		c.GatewayName = defaultGatewayName
	}
	if c.GatewayNamespace == "" {
		c.GatewayNamespace = c.Namespace
	}
	if c.ActivatorService == "" {
		c.ActivatorService = defaultActivatorService
	}
	if c.ActivatorPort <= 0 {
		c.ActivatorPort = defaultActivatorPort
	}
	if c.ActivatorSelector == "" {
		c.ActivatorSelector = defaultActivatorSelector
	}
	if c.RevisionHistoryLimit <= 0 {
		c.RevisionHistoryLimit = defaultRevisionHistoryLimit
	}
}
