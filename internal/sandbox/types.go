// Package sandbox defines the sandbox domain: a live, isolated workspace you
// drive from the outside — create one, run commands in it, read and write its
// files, tear it down. Where a job runs to completion and a deployment serves
// traffic under a stable name, a sandbox does neither; it waits for you.
//
// A sandbox is created from a warm pool (pkg/pool), so creation is a claim
// rather than a container start. Exec and files are NOT part of this API: they
// are an HTTP contract the sandbox IMAGE serves at the sandbox's own URL
// (open-runtimes/sandbox is the reference image), which keeps the control plane
// off the data path. See docs/sandboxes.md.
package sandbox

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/pool"
	"orchestrator/internal/volume"
)

// Request creates a sandbox.
type Request struct {
	// ID is caller-chosen for a stable API path and idempotency (re-POSTing an
	// existing id is 409); generated when empty. It is NOT the address — the
	// URL carries an unguessable token instead, precisely because a
	// caller-chosen id is guessable.
	ID string `json:"id,omitempty"`
	// Command is the payload the claim execs; empty falls back to the pool's,
	// which is the usual case (the pool's image already serves the contract).
	Command     string            `json:"command,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	// Ports are extra ports this sandbox serves, beyond the pool's own. Each
	// gets its own hostname, so a caller can reach a dev server, an LSP, or a
	// terminal socket alongside the contract. Unlike volumes and the isolation
	// tier, ports are NOT fixed by the warm pod: a container may bind any port
	// at any time, so this stays a per-sandbox field.
	Ports     []int        `json:"ports,omitempty"`
	Artifacts artifact.Set `json:"artifacts,omitempty"` // materialized into the workspace before ready
	// TimeoutSeconds bounds each request to the sandbox's URL. Omitted takes
	// the default (300); 0 means NO bound, for sessions meant to outlive one —
	// a WebSocket terminal, a language server, a long stream. A plain int could
	// not carry that: omitted and "0" are the same value on the wire.
	TimeoutSeconds     *int `json:"timeoutSeconds,omitempty"`
	IdleTimeoutSeconds int  `json:"idleTimeoutSeconds,omitempty"` // tear down after this long with no traffic; 0 = until DELETE

	// Pool names the sandbox pool to claim from — the fast path: a warm pod is
	// already running, so a create is a claim. Leave it empty and declare an
	// Image instead for a sandbox with no pool behind it: the pod is created on
	// demand, which costs a cold start but needs no standing capacity and takes
	// its shape from this request rather than from an operator's config.
	//
	// Exactly one of Pool or Image.
	Pool string `json:"pool,omitempty"`

	// Image is the runtime image for a poolless sandbox. The agent is installed
	// into it exactly as it is into a pool's image, so any image works.
	Image string `json:"image,omitempty"`
	// Port is where the contract is served in a poolless sandbox — what a pool
	// would otherwise declare. Required with Image.
	Port int `json:"port,omitempty"`
	// CPU and Memory size a poolless sandbox's workload container. Zero takes
	// the platform default, as a pool's would.
	CPU    float64 `json:"cpu,omitempty"`
	Memory int     `json:"memory,omitempty"`
	// RuntimeClass is the isolation tier for a poolless sandbox (runc | gvisor |
	// kata). Unlike a pool's, this is per-sandbox: the pod is built for this
	// request, so nothing was fixed before it arrived.
	RuntimeClass string `json:"runtimeClass,omitempty"`
	// Volumes attach existing storage to a poolless sandbox. Also per-sandbox
	// for the same reason — a pool cannot do this because its pods are already
	// running when you claim one.
	Volumes []volume.Volume `json:"volumes,omitempty"`

	// Token is the capability the sandbox's hostname carries — minted by the
	// service, never accepted from or echoed back into a request body.
	Token string `json:"-"`
}

// Sandbox states.
const (
	StateCreating = "creating" // claimed; artifacts materializing
	StateReady    = "ready"    // the contract is served at its URL
	StateFailed   = "failed"   // artifacts failed, or the image never became ready
	StateDeleting = "deleting" // teardown in progress
)

// MetricKind labels this consumer's warm-pool telemetry, distinguishing
// sandbox pools from deployment pools in the shared pool_* series.
const MetricKind = "sandbox"

// Status is the API view of a sandbox.
//
// URL is a CAPABILITY: anyone who can reach it can run commands in the
// sandbox, so it must stay out of logs, error bodies, and event payloads.
type Status struct {
	ID string `json:"id"`
	// PoolID names the pool it was claimed from, absent for a poolless sandbox.
	PoolID string `json:"poolId,omitempty"`
	State  string `json:"status"` // creating|ready|failed|deleting
	URL    string `json:"url,omitempty"`
	// URLs addresses every port the sandbox serves, keyed by port number
	// (including the pool's own). Present so callers never build a hostname
	// themselves — the token in it is not derivable from the id.
	URLs  map[string]string `json:"urls,omitempty"`
	Error string            `json:"error,omitempty"`
}

// ListResponse is the response for listing live sandboxes.
type ListResponse struct {
	Sandboxes []Status `json:"sandboxes"`
}

// LoadPools parses the SANDBOX_POOLS_JSON config value. Sandbox pools are a
// separate fleet from deployment pools: their image must serve the sandbox
// contract, and their pods are reached by wildcard rather than a per-workload
// route.
func LoadPools(raw string) ([]pool.Pool, error) {
	return pool.Load(raw, "SANDBOX_POOLS_JSON")
}
