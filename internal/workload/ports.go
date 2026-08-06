package workload

// Well-known ports — the contract between backends (which wire containers),
// the sidecar itself, and the activator (which forwards to ProxyPort and probes
// AdminPort). Reserved: a workload may not ask for either.
const (
	DefaultProxyPort = 8000 // data: proxied traffic
	DefaultAdminPort = 8001 // admin: /ready (kubelet + activator probes), /stats (autoscaler scrape)
)

// MountsReadyPath reports whether the sidecar has established the workload's
// image mounts: 200 once it has, 503 until then. A direct-mode workload's
// startup probe gates on it, so the kubelet does not start the container that
// reads those mounts until they are in place.
const MountsReadyPath = "/mounts-ready"

// Environment variable names — the contract stamped by backends into sidecar
// containers.
const (
	EnvTarget                    = "PROXY_TARGET"      // host:port of the user container
	EnvExtraPorts                = "PROXY_EXTRA_PORTS" // comma-separated secondary ports, reachable via HeaderPort
	EnvProxyPort                 = "PROXY_PORT"
	EnvAdminPort                 = "PROXY_ADMIN_PORT"
	EnvTimeoutSeconds            = "PROXY_TIMEOUT_SECONDS" // per-request total → 504
	EnvMaxDrainSeconds           = "PROXY_MAX_DRAIN_SECONDS"
	EnvConcurrency               = "PROXY_CONCURRENCY"    // hard in-flight cap; 0 = unlimited
	EnvQueueSize                 = "PROXY_QUEUE_SIZE"     // pending queue above the cap; overflow → 503
	EnvReadinessPath             = "PROXY_READINESS_PATH" // HTTP GET path; empty = TCP connect
	EnvReadinessPeriodMillis     = "PROXY_READINESS_PERIOD_MS"
	EnvReadinessTimeoutMillis    = "PROXY_READINESS_TIMEOUT_MS"
	EnvReadinessFailureThreshold = "PROXY_READINESS_FAILURE_THRESHOLD"

	// EnvArtifacts carries the workload's artifacts as JSON. The phase that
	// materializes them reads it; a sidecar that must also MOUNT one reads it to
	// know what to mount.
	EnvArtifacts = "ARTIFACTS_JSON"

	// EnvMounts tells a pool-mode sidecar its pod can establish image mounts:
	// it runs privileged and the workspace carries propagation. Set only for
	// pools that declared the capability, so a claim asking to mount without it
	// fails in the API rather than with EPERM in the pod.
	EnvMounts = "PROXY_MOUNTS"

	// EnvTargetHost is the pool-mode host the claimed workload serves on; the
	// claim's Port is joined to it. Docker warm containers front a sibling
	// container, not localhost — hence a distinct knob from EnvTarget.
	EnvTargetHost = "PROXY_TARGET_HOST"
)
