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

// activationJSON mirrors Activation with json.RawMessage artifacts (same
// pattern as pkg/job and pkg/deployment).
type activationJSON struct {
	ID                 string               `json:"id,omitempty"`
	Host               string               `json:"host,omitempty"`
	Command            string               `json:"command"`
	Environment        map[string]string    `json:"environment,omitempty"`
	Artifacts          json.RawMessage      `json:"artifacts,omitempty"`
	TimeoutSeconds     int                  `json:"timeoutSeconds,omitempty"`
	IdleTimeoutSeconds int                  `json:"idleTimeoutSeconds,omitempty"`
	Callback           *deployment.Callback `json:"callback,omitempty"`
}

// Parse decodes an API request body, rejecting unknown fields — a typo'd
// field name must fail loudly, not silently activate with defaults.
// Strictness belongs at the API edge only: stored specs (pod annotations)
// keep the lenient UnmarshalJSON so version skew never strands them.
func Parse(data []byte) (*Activation, error) {
	var raw activationJSON
	if err := artifact.UnmarshalStrict(data, &raw); err != nil {
		return nil, err
	}
	var a Activation
	if err := a.fromRaw(&raw); err != nil {
		return nil, err
	}
	return &a, nil
}

// UnmarshalJSON decodes artifacts into their concrete types via the registry
// — a plain decode cannot land on the artifact.Artifact interface.
func (a *Activation) UnmarshalJSON(data []byte) error {
	var raw activationJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	return a.fromRaw(&raw)
}

func (a *Activation) fromRaw(raw *activationJSON) error {
	a.ID = raw.ID
	a.Host = raw.Host
	a.Command = raw.Command
	a.Environment = raw.Environment
	a.TimeoutSeconds = raw.TimeoutSeconds
	a.IdleTimeoutSeconds = raw.IdleTimeoutSeconds
	a.Callback = raw.Callback

	if len(raw.Artifacts) > 0 && string(raw.Artifacts) != "null" {
		artifacts, err := artifact.UnmarshalArtifacts(raw.Artifacts)
		if err != nil {
			return fmt.Errorf("failed to unmarshal artifacts: %w", err)
		}
		a.Artifacts = artifacts
	}
	return nil
}

// MarshalJSON stamps each artifact with its "type" discriminator via the
// registry — required for the round trip through pod annotations.
func (a Activation) MarshalJSON() ([]byte, error) {
	raw := activationJSON{
		ID:                 a.ID,
		Host:               a.Host,
		Command:            a.Command,
		Environment:        a.Environment,
		TimeoutSeconds:     a.TimeoutSeconds,
		IdleTimeoutSeconds: a.IdleTimeoutSeconds,
		Callback:           a.Callback,
	}
	if len(a.Artifacts) > 0 {
		data, err := artifact.MarshalArtifacts(a.Artifacts)
		if err != nil {
			return nil, err
		}
		raw.Artifacts = data
	}
	return json.Marshal(raw)
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
