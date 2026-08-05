package proxy

import (
	"orchestrator/internal/artifact"
)

// The claim protocol — the contract between the pool backends (which POST
// activations), this sidecar (the pod's serialization point: it accepts
// exactly one), and the pool-shim (which it signals over the FIFO). See
// docs/pools.md.
const (
	// EnvClaimToken arms pool mode: when set, the sidecar starts unclaimed
	// (no proxy target, not ready) and exposes the claim endpoints, requiring
	// this bearer token. API auth is NOT bypassable in-cluster: without the
	// token the surface is 401.
	EnvClaimToken = "POOL_CLAIM_TOKEN"

	// ClaimPath accepts the activation: POST, Authorization: Bearer <token>.
	// 401 bad token; 409 already claimed (the racing loser retries the next
	// warm pod); 422 artifacts failed (the pod is poisoned — never claimable
	// again, reported failed until discarded); 200 claimed and signaled.
	ClaimPath = "/activate"
	// ClaimStatePath reports the sidecar's claim state: GET → ClaimState.
	// The reconcile / orphan-GC source of truth.
	ClaimStatePath = "/activation"

	// ShimFIFOName is the FIFO (relative to the shared workspace) the shim
	// blocks on; the sidecar writes one ShimExec JSON line to trigger the exec.
	ShimFIFOName = ".pool-exec.fifo"

	// HeaderPort names which of the claim's ports a request is for. Set by the
	// sandbox edge from the request's hostname and NEVER trusted from a client
	// — the edge strips any inbound copy. The port is dialed on loopback inside
	// the claimed pod, so it can only ever reach that pod's own listeners, and
	// only ports the claim declared.
	HeaderPort = "X-Sandbox-Port"
)

// ClaimRequest is what a pool backend POSTs to claim the pod.
type ClaimRequest struct {
	ActivationID string            `json:"activationId"`
	Command      string            `json:"command"`
	Environment  map[string]string `json:"environment,omitempty"`
	Artifacts    artifact.Set      `json:"artifacts,omitempty"`
	Port         int               `json:"port"` // the container port the activation serves — the proxy target + readiness subject
	// Ports are additional ports the workload serves, reachable via HeaderPort.
	// Only Port is probed for readiness: a secondary port may come up late (a
	// dev server started from an exec) without ever failing the sandbox.
	Ports []int `json:"ports,omitempty"`
	// TimeoutSeconds bounds each proxied request. Nil leaves the sidecar's own
	// configured default in place; a value of 0 is an EXPLICIT "no bound", for
	// sessions that are meant to outlive any timeout — a WebSocket terminal, a
	// language server, a long stream. The pointer is what separates "the caller
	// did not say" from "the caller said unbounded"; artifact materialization
	// keeps the sidecar's budget either way.
	TimeoutSeconds *int `json:"timeoutSeconds,omitempty"`
}

// ClaimState is the sidecar's authoritative claim record.
type ClaimState struct {
	Claimed      bool   `json:"claimed"`
	ActivationID string `json:"activationId,omitempty"`
	Failed       bool   `json:"failed,omitempty"` // artifacts/signal failed; pod is poisoned
	Error        string `json:"error,omitempty"`
}

// ShimExec is the single JSON line written to the shim FIFO.
type ShimExec struct {
	Command     string            `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	WorkDir     string            `json:"workDir,omitempty"`
}
