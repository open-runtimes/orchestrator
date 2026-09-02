// Package proxy is the workload-sidecar: a reverse proxy fronting the user
// container in every deployment replica and sandbox. Traffic
// reaches the container only through it. It owns the pod-local invariants:
// readiness gating, graceful drain, per-request timeout, and the hard
// concurrency cap. See docs/deployments.md.
//
// This is the IMPLEMENTATION. The contract — ports, environment, headers, the
// claim endpoints and payloads — is internal/workload, which everything else
// imports instead: only the binary that serves the sidecar needs what is here.
package proxy

import (
	"orchestrator/internal/config"
	"orchestrator/internal/workload"
	"strconv"
	"strings"
	"time"
)

// Config configures the proxy.
type Config struct {
	Target string // host:port of the user container
	// ExtraPorts are secondary ports on the target host, addressable via
	// workload.HeaderPort. Direct mode takes them from the environment; pool mode takes
	// them from the claim.
	ExtraPorts []int
	ProxyPort  int
	AdminPort  int

	// Mounts reports that this pod can establish image mounts: the sidecar runs
	// privileged and the workspace carries propagation. Set by the backend for
	// pools that declared the capability, and for a direct-mode workload whose
	// request carries a mount artifact.
	Mounts bool
	// ArtifactsJSON is the direct-mode workload's artifacts. Only the mounts in
	// it concern this sidecar — the rest were materialized by the phase that ran
	// before it. Empty in pool mode, where the claim carries them instead.
	ArtifactsJSON string

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
		Target:    config.GetEnv(workload.EnvTarget, ""),
		ProxyPort: config.GetIntEnv(workload.EnvProxyPort, workload.DefaultProxyPort),
		AdminPort: config.GetIntEnv(workload.EnvAdminPort, workload.DefaultAdminPort),

		Mounts:        config.GetEnv(workload.EnvMounts, "") == "true",
		ArtifactsJSON: config.GetEnv(workload.EnvArtifacts, ""),
		Timeout:       time.Duration(config.GetIntEnv(workload.EnvTimeoutSeconds, 300)) * time.Second,
		MaxDrain:      time.Duration(config.GetIntEnv(workload.EnvMaxDrainSeconds, 90)) * time.Second,

		Concurrency: config.GetIntEnv(workload.EnvConcurrency, 0),
		QueueSize:   config.GetIntEnv(workload.EnvQueueSize, 100),

		ReadinessPath:             config.GetEnv(workload.EnvReadinessPath, ""),
		ReadinessPeriod:           time.Duration(config.GetIntEnv(workload.EnvReadinessPeriodMillis, 100)) * time.Millisecond,
		ReadinessTimeout:          time.Duration(config.GetIntEnv(workload.EnvReadinessTimeoutMillis, 1000)) * time.Millisecond,
		ReadinessFailureThreshold: config.GetIntEnv(workload.EnvReadinessFailureThreshold, 3),

		ClaimToken: config.GetEnv(workload.EnvClaimToken, ""),
		TargetHost: config.GetEnv(workload.EnvTargetHost, "127.0.0.1"),
		ExtraPorts: parsePorts(config.GetEnv(workload.EnvExtraPorts, "")),
		Workspace:  config.Workspace(),
		S3:         config.LoadS3Credentials(),
	}
}

// parsePorts reads a comma-separated port list, skipping anything unparseable —
// a malformed entry must not take the sidecar down, it just is not routable.
func parsePorts(raw string) []int {
	var ports []int
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if port, err := strconv.Atoi(entry); err == nil && port > 0 && port <= 65535 {
			ports = append(ports, port)
		}
	}
	return ports
}
