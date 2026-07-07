// Package proxy is the deployments-sidecar: a reverse proxy fronting the user
// container in every deployment replica. Traffic reaches the container only
// through it. It owns the pod-local invariants: readiness gating, graceful
// drain, per-request timeout, and the hard concurrency cap. See
// docs/deployments.md.
package proxy

import (
	"orchestrator/internal/config"
	"time"
)

// Well-known ports — the contract between backends (which wire containers),
// this proxy, and the activator (which forwards to ProxyPort and probes
// AdminPort).
const (
	DefaultProxyPort = 8000 // data: proxied traffic
	DefaultAdminPort = 8001 // admin: /ready (kubelet + activator probes), /stats (autoscaler scrape)
)

// Environment variable names — the contract stamped by backends into proxy
// containers.
const (
	EnvTarget                    = "PROXY_TARGET" // host:port of the user container
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

	// EnvTargetHost is the pool-mode host the claimed workload serves on; the
	// claim's Port is joined to it. Docker warm containers front a sibling
	// container, not localhost — hence a distinct knob from EnvTarget.
	EnvTargetHost = "PROXY_TARGET_HOST"
)

// Config configures the proxy.
type Config struct {
	Target    string // host:port of the user container
	ProxyPort int
	AdminPort int

	Timeout  time.Duration // per-request total → 504
	MaxDrain time.Duration // cap on drain wait at shutdown

	Concurrency int // hard in-flight cap; 0 = unlimited
	QueueSize   int // pending queue above the cap; overflow → 503

	ReadinessPath             string // HTTP GET path on Target; empty = TCP connect
	ReadinessPeriod           time.Duration
	ReadinessTimeout          time.Duration
	ReadinessFailureThreshold int

	// Pool mode (see claim.go) — armed when ClaimToken is set: the proxy
	// starts unclaimed with no target, and POST /activate late-binds one.
	// Target is ignored in pool mode.
	ClaimToken string // bearer token required to claim; empty = direct mode
	TargetHost string // host the claimed workload serves on, joined with the claim's Port
	Workspace  string // shared volume: artifact root + shim FIFO directory

	// S3 credentials for signing s3:// download artifacts materialized into the
	// workspace. Forwarded by the deployments/pools orchestrator.
	S3 config.S3Credentials
}

// LoadConfigFromEnv loads proxy configuration from the environment.
func LoadConfigFromEnv() Config {
	return Config{
		Target:    config.GetEnv(EnvTarget, ""),
		ProxyPort: config.GetIntEnv(EnvProxyPort, DefaultProxyPort),
		AdminPort: config.GetIntEnv(EnvAdminPort, DefaultAdminPort),

		Timeout:  time.Duration(config.GetIntEnv(EnvTimeoutSeconds, 300)) * time.Second,
		MaxDrain: time.Duration(config.GetIntEnv(EnvMaxDrainSeconds, 90)) * time.Second,

		Concurrency: config.GetIntEnv(EnvConcurrency, 0),
		QueueSize:   config.GetIntEnv(EnvQueueSize, 100),

		ReadinessPath:             config.GetEnv(EnvReadinessPath, ""),
		ReadinessPeriod:           time.Duration(config.GetIntEnv(EnvReadinessPeriodMillis, 100)) * time.Millisecond,
		ReadinessTimeout:          time.Duration(config.GetIntEnv(EnvReadinessTimeoutMillis, 1000)) * time.Millisecond,
		ReadinessFailureThreshold: config.GetIntEnv(EnvReadinessFailureThreshold, 3),

		ClaimToken: config.GetEnv(EnvClaimToken, ""),
		TargetHost: config.GetEnv(EnvTargetHost, "127.0.0.1"),
		Workspace:  config.GetEnv("SHARED_VOLUME_PATH", "/workspace"), // same contract as the shim and job sidecar
		S3:         config.LoadS3Credentials(),
	}
}
