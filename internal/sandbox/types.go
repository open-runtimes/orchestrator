// Package sandbox defines the sandbox domain: a live, isolated workspace you
// drive from the outside — create one, run commands in it, read and write its
// files, tear it down. Where a job runs to completion and a deployment serves
// traffic under a stable name, a sandbox does neither; it waits for you.
//
// A sandbox declares a complete pod shape and transparently claims matching
// warm capacity when available, otherwise creating directly. Exec and files are
// NOT part of this API: they
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
	// Ports are extra ports this sandbox serves, beyond its primary port. Each
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

	// Pool is the operator pool selected by exact fixed-shape matching. It is
	// internal coordination state, never accepted from or exposed to API users.
	Pool string `json:"-"`

	// Image is the runtime image. The agent is installed into it, so any image
	// works. Required for every API request.
	Image string `json:"image,omitempty"`
	// Port is where the contract is served. Required with Image.
	Port int `json:"port,omitempty"`
	// CPU and Memory size the sandbox's workload container. Zero takes the
	// platform default.
	CPU    float64 `json:"cpu,omitempty"`
	Memory int     `json:"memory,omitempty"`
	// RuntimeClass is the isolation tier (runc | gvisor | kata).
	RuntimeClass string `json:"runtimeClass,omitempty"`
	// TerminationGracePeriodSeconds bounds teardown: the
	// drain, the post-phase artifacts, and the unmount happen inside it. Raise it
	// if this sandbox snapshots itself on the way out.
	TerminationGracePeriodSeconds int `json:"terminationGracePeriodSeconds,omitempty"`

	// Volumes attach existing storage and participate in exact pool matching.
	Volumes []volume.Volume `json:"volumes,omitempty"`

	// Token is the capability the sandbox's hostname carries — minted by the
	// service, never accepted from or echoed back into a request body.
	Token string `json:"-"`
}

// Shape is the pod this request describes. The fields below are exactly those
// a warm pod must already share to be claimable. Mounting is inferred from the
// artifacts, as it is for a job or a revision, because the pod is built for
// this request.
func (r *Request) Shape() pool.Spec {
	return pool.Spec{
		Image:                         r.Image,
		Port:                          r.Port,
		CPU:                           r.CPU,
		Memory:                        r.Memory,
		RuntimeClass:                  r.RuntimeClass,
		Volumes:                       r.Volumes,
		TerminationGracePeriodSeconds: r.TerminationGracePeriodSeconds,
		Mounts:                        artifact.HasMount(r.Artifacts),
	}
}

// Recorded returns a copy of the request carrying the shape the sandbox was
// actually built in, so what is stored describes the pod rather than the ask.
// A pooled sandbox names no shape of its own — its pool holds one — and that
// pool may be re-imaged or dropped while the pod keeps running, which would
// leave a reader with the replacement or with nothing at all.
func (r *Request) Recorded(shape pool.Spec) *Request {
	recorded := *r
	recorded.Image = shape.Image
	recorded.Port = shape.Port
	recorded.CPU = shape.CPU
	recorded.Memory = shape.Memory
	recorded.RuntimeClass = shape.RuntimeClass

	return &recorded
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
	// PoolID is retained internally for backend reconstruction and diagnostics;
	// pool identity is operator detail and is not part of the public API.
	PoolID string `json:"-"`
	State  string `json:"status"` // creating|ready|failed|deleting
	URL    string `json:"url,omitempty"`
	// URLs addresses every port the sandbox serves, keyed by port number
	// (including the primary). Present so callers never build a hostname
	// themselves — the token in it is not derivable from the id.
	URLs map[string]string `json:"urls,omitempty"`
	// Image, CPU and Memory are the shape the sandbox is running in — read
	// back off the pool it was claimed from, or off the request that built its
	// pod. A caller that did not create this sandbox, or has forgotten what it
	// asked for, can see what it got without keeping a record of its own.
	Image  string  `json:"image,omitempty"`
	CPU    float64 `json:"cpu,omitempty"`
	Memory int     `json:"memory,omitempty"`
	Error  string  `json:"error,omitempty"`
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
	pools, err := pool.Load(raw, "SANDBOX_POOLS_JSON")
	if err != nil {
		return nil, err
	}
	if err := pool.ValidateTransparent(pools, "sandbox"); err != nil {
		return nil, err
	}
	return pools, nil
}
