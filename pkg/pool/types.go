// Package pool defines the warm-pool domain: config-declared fleets of a
// runtime image kept idle, onto which an activation late-binds a payload —
// claim + inject + exec instead of schedule + pull + start. See
// docs/design/pools.md.
package pool

import (
	"encoding/json"
	"fmt"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/deployment"
)

// Pool is the service-config schema (loaded at startup from POOLS_JSON, which
// Helm renders from a `pools:` list). A pool is standing warm capacity —
// adding, resizing, or removing one is a config change + rollout, not a
// runtime call, so the API over pools is read + activate only.
type Pool struct {
	ID          string             `json:"id"`
	Image       string             `json:"image"`
	Sandbox     string             `json:"sandbox,omitempty"` // RuntimeClass tier: runc (default) | gvisor | kata (K8s only). A pool dimension — warm pods are runtime-fixed at creation, so warm fleets are keyed by (image, sandbox).
	Size        int                `json:"size"`              // warm pods kept ready
	CPU         float64            `json:"cpu"`
	Memory      int                `json:"memory"`
	Port        int                `json:"port,omitempty"` // >0: HTTP pool (activations serve on it); 0: exec pool (run to completion)
	Probes      *deployment.Probes `json:"probes,omitempty"`
	Environment map[string]string  `json:"environment,omitempty"`
	Meta        map[string]string  `json:"meta,omitempty"`

	// Burst controls what happens when an activation arrives and no warm pod
	// is free: "reject" (default) → 429; "cold" → create a pod on demand and
	// pay the cold start. Always logged either way.
	Burst string `json:"burst,omitempty"`
}

// Burst policies.
const (
	BurstReject = "reject"
	BurstCold   = "cold"
)

// HTTP reports whether activations of this pool serve HTTP (vs run to
// completion).
func (p *Pool) HTTP() bool { return p.Port > 0 }

// LoadPools parses the POOLS_JSON config value.
func LoadPools(raw string) ([]Pool, error) {
	if raw == "" {
		return nil, nil
	}
	var pools []Pool
	if err := json.Unmarshal([]byte(raw), &pools); err != nil {
		return nil, fmt.Errorf("invalid POOLS_JSON: %w", err)
	}
	seen := make(map[string]bool, len(pools))
	for i := range pools {
		p := &pools[i]
		if p.ID == "" || p.Image == "" {
			return nil, fmt.Errorf("pool %d: id and image are required", i)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("duplicate pool id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Size <= 0 {
			p.Size = 1
		}
		if p.Burst == "" {
			p.Burst = BurstReject
		}
		if p.Burst != BurstReject && p.Burst != BurstCold {
			return nil, fmt.Errorf("pool %q: burst must be %q or %q", p.ID, BurstReject, BurstCold)
		}
		if !deployment.ValidSandbox(p.Sandbox) {
			return nil, fmt.Errorf("pool %q: sandbox must be one of %q, %q, %q",
				p.ID, deployment.SandboxRunc, deployment.SandboxGvisor, deployment.SandboxKata)
		}
	}
	return pools, nil
}

// Activation is the runtime request late-bound onto a warm pod.
type Activation struct {
	ID                 string               `json:"id,omitempty"`   // caller-chosen (stable URL), RFC-1123 label; else generated
	Host               string               `json:"host,omitempty"` // HTTP pools: RFC-1123 hostname; else {id}.{pool-domain}
	Command            string               `json:"command"`
	Environment        map[string]string    `json:"environment,omitempty"`
	Artifacts          []artifact.Artifact  `json:"artifacts,omitempty"`
	TimeoutSeconds     int                  `json:"timeoutSeconds,omitempty"`     // exec: run bound; HTTP: per-request bound
	IdleTimeoutSeconds int                  `json:"idleTimeoutSeconds,omitempty"` // HTTP: tear down after idleness; 0 = until DELETE
	Callback           *deployment.Callback `json:"callback,omitempty"`
}

// Activation states.
const (
	StateActivating   = "activating"
	StateReady        = "ready"
	StateExited       = "exited"
	StateFailed       = "failed"
	StateDeactivating = "deactivating"
)

// ActivationStatus is the API view of an activation. ExitCode/Output apply
// only to exec pools; URL only to HTTP pools.
type ActivationStatus struct {
	ID       string `json:"id"`
	PoolID   string `json:"poolId"`
	PodID    string `json:"podId,omitempty"`
	URL      string `json:"url,omitempty"`
	State    string `json:"status"` // activating|ready|exited|failed|deactivating
	ExitCode *int   `json:"exitCode,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Status is the API view of a configured pool.
type Status struct {
	ID      string `json:"id"`
	Image   string `json:"image"`
	Size    int    `json:"size"`
	Warm    int    `json:"warm"`    // unclaimed, warm-ready pods
	Claimed int    `json:"claimed"` // pods bound to an activation
}
