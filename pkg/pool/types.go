// Package pool defines the warm-pool domain: config-declared fleets of a
// runtime image kept idle, onto which an activation late-binds a payload —
// claim + inject + exec instead of schedule + pull + start. See
// docs/pools.md.
package pool

import (
	"encoding/json"
	"fmt"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/deployment"
	"orchestrator/pkg/volume"
)

// Pool is the service-config schema (loaded at startup from POOLS_JSON, which
// Helm renders from a `pools:` list). A pool is standing warm capacity —
// adding, resizing, or removing one is a config change + rollout, not a
// runtime call, so the API over pools is read + activate only.
type Pool struct {
	ID           string             `json:"id"`
	Image        string             `json:"image"`
	RuntimeClass string             `json:"runtimeClass,omitempty"` // isolation tier: runc (default) | gvisor | kata (K8s only). A pool dimension — warm pods are runtime-fixed at creation, so warm fleets are keyed by (image, runtimeClass).
	Size         int                `json:"size"`                   // warm pods kept ready
	CPU          float64            `json:"cpu"`
	Memory       int                `json:"memory"`
	Port         int                `json:"port"` // required — the container port activations serve HTTP on
	Probes       *deployment.Probes `json:"probes,omitempty"`
	Environment  map[string]string  `json:"environment,omitempty"`
	Meta         map[string]string  `json:"meta,omitempty"`
	Volumes      []volume.Volume    `json:"volumes,omitempty"` // existing K8s PVCs mounted into every warm pod in the fleet

	// Burst controls what happens when an activation arrives and no warm pod
	// is free: "cold" (default) → create a pod on demand and pay the cold
	// start; "reject" → 429. Always logged either way.
	Burst string `json:"burst,omitempty"`
}

// Burst policies.
const (
	BurstReject = "reject"
	BurstCold   = "cold"
)

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
		if p.Port <= 0 {
			return nil, fmt.Errorf("pool %q: port is required", p.ID)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("duplicate pool id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Size <= 0 {
			p.Size = 1
		}
		if p.Burst == "" {
			p.Burst = BurstCold
		}
		if p.Burst != BurstReject && p.Burst != BurstCold {
			return nil, fmt.Errorf("pool %q: burst must be %q or %q", p.ID, BurstReject, BurstCold)
		}
		if !deployment.ValidRuntimeClass(p.RuntimeClass) {
			return nil, fmt.Errorf("pool %q: runtimeClass must be one of %q, %q, %q",
				p.ID, deployment.RuntimeClassRunc, deployment.RuntimeClassGvisor, deployment.RuntimeClassKata)
		}
		for j, v := range p.Volumes {
			if err := v.Validate(fmt.Sprintf("pool %q volumes[%d]", p.ID, j)); err != nil {
				return nil, err
			}
		}
	}
	return pools, nil
}

// Activation is the runtime request late-bound onto a warm pod.
type Activation struct {
	ID                 string               `json:"id,omitempty"`   // caller-chosen (stable URL), RFC-1123 label; else generated
	Host               string               `json:"host,omitempty"` // RFC-1123 hostname; else {id}.{pool-domain}
	Command            string               `json:"command"`
	Environment        map[string]string    `json:"environment,omitempty"`
	Artifacts          artifact.Set         `json:"artifacts,omitempty"`
	TimeoutSeconds     int                  `json:"timeoutSeconds,omitempty"`     // per-request bound → 504
	IdleTimeoutSeconds int                  `json:"idleTimeoutSeconds,omitempty"` // tear down after idleness; 0 = until DELETE
	Callback           *deployment.Callback `json:"callback,omitempty"`
}

// Parse decodes an API request body, rejecting unknown fields — a typo'd
// field name must fail loudly, not silently activate with defaults.
// Strictness belongs at the API edge only: stored specs (pod annotations)
// decode leniently so version skew never strands them.
func Parse(data []byte) (*Activation, error) {
	var a Activation
	if err := artifact.UnmarshalStrict(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Activation states.
const (
	StateActivating   = "activating"
	StateReady        = "ready"
	StateFailed       = "failed"
	StateDeactivating = "deactivating"
)

// ActivationStatus is the API view of an activation.
type ActivationStatus struct {
	ID     string `json:"id"`
	PoolID string `json:"poolId"`
	PodID  string `json:"podId,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"status"` // activating|ready|failed|deactivating
	Error  string `json:"error,omitempty"`
}

// Status is the API view of a configured pool.
type Status struct {
	ID      string `json:"id"`
	Image   string `json:"image"`
	Size    int    `json:"size"`
	Warm    int    `json:"warm"`    // unclaimed, warm-ready pods
	Claimed int    `json:"claimed"` // pods bound to an activation
}
