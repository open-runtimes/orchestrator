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
)

// ClaimRequest is what a pool backend POSTs to claim the pod.
type ClaimRequest struct {
	ActivationID   string              `json:"activationId"`
	Command        string              `json:"command"`
	Environment    map[string]string   `json:"environment,omitempty"`
	Artifacts      []artifact.Artifact `json:"artifacts,omitempty"`
	Port           int                 `json:"port"` // the container port the activation serves — the proxy target + readiness subject
	TimeoutSeconds int                 `json:"timeoutSeconds,omitempty"`
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
