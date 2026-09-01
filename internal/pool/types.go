// Package pool defines the warm-pool domain: config-declared pools of a
// runtime image kept idle, onto which a claim late-binds a payload — claim +
// inject + exec instead of schedule + pull + start. The Pool declaration is
// shared by every consumer of standing warm capacity (deployment Revisions
// and sandboxes). See docs/pools.md.
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/claim"
	"orchestrator/internal/deployment"
	"orchestrator/internal/volume"
)

// Spec is the pod shape: what a claimed workload runs in — image, port,
// resources, isolation tier, storage. A pool declares one and stamps it into
// every warm pod of its fleet; a workload with no pool behind it (a poolless
// sandbox) carries its own, because its pod is built for that one request.
//
// It is separate from the capacity policy on Pool below (size, burst, idle
// ceiling) because the two have different audiences: the backends that build
// pods read only the shape, and the claim protocol reads only the policy.
// Embedded rather than nested, so the config and API wire formats stay flat.
type Spec struct {
	Image string `json:"image"`
	// Command is the payload a claim execs when the request does not name one.
	// Sandbox pools may set it as their default; deployment Revisions may
	// late-bind their own command instead.
	Command string `json:"command,omitempty"`
	// RuntimeClass is the isolation tier: runc (default) | gvisor | kata. Fixed
	// for a pool, because warm pods are runtime-fixed at creation, so warm pools
	// are keyed by (image, runtimeClass); per-request for a poolless workload.
	RuntimeClass string            `json:"runtimeClass,omitempty"`
	CPU          float64           `json:"cpu"`
	Memory       int               `json:"memory"`
	Port         int               `json:"port"` // required — the container port the claimed workload serves HTTP on
	Environment  map[string]string `json:"environment,omitempty"`
	Volumes      []volume.Volume   `json:"volumes,omitempty"` // existing K8s PVCs mounted into the worker container

	// Mounts lets a claim against this shape establish image mounts (the mount
	// artifact). It belongs to the SHAPE because it changes the pod: the sidecar
	// performing the mount runs privileged as root, and the shared workspace
	// carries mount propagation. Off by default, and worth leaving off — a
	// privileged container sits in every pod, beside whatever the claim runs.
	Mounts bool `json:"mounts,omitempty"`

	// TerminationGracePeriodSeconds bounds teardown: the sidecar drains, runs the
	// claim's post-phase artifacts, and releases its mounts inside it. Kubernetes
	// defaults to 30 seconds, which is not enough to archive and upload a
	// workspace of any size — so a shape whose claims snapshot on shutdown must
	// raise it.
	TerminationGracePeriodSeconds int `json:"terminationGracePeriodSeconds,omitempty"`
}

// Pool is the service-config schema for one warm pool (Helm renders it from a
// `pools:` list): a Spec plus the capacity policy for the fleet standing in
// that shape. A pool is standing capacity — adding, resizing, or removing one
// is a config change plus a rollout, not a runtime call, so the API over pools
// is read-only.
type Pool struct {
	ID   string `json:"id"`
	Spec        // the pod every warm pod in this pool stands in; flattened on the wire

	Size int `json:"size"` // warm pods kept ready

	// Burst controls what happens when a claim arrives and no warm pod is
	// free: "cold" (default) → create a pod on demand and pay the cold start;
	// "reject" → 429. Always logged either way.
	Burst string `json:"burst,omitempty"`

	// MaxIdleSeconds caps a claim's requested idle timeout (0 = uncapped).
	// Sandbox pools want one: an abandoned sandbox holds a warm pod hostage.
	MaxIdleSeconds int `json:"maxIdleSeconds,omitempty"`
}

// MetricKind labels this consumer's warm-pool telemetry, distinguishing
// deployment pools from sandbox pools in the shared pool_* series.
const MetricKind = "pool"

// Burst policies, owned by the claim protocol that implements them.
const (
	BurstReject = claim.BurstReject
	BurstCold   = claim.BurstCold
)

// LoadPools parses the POOLS_JSON config value.
func LoadPools(raw string) ([]Pool, error) {
	return Load(raw, "POOLS_JSON")
}

// Load parses a pool list from config. source names the environment variable
// it came from, so a malformed value points at its own knob.
func Load(raw, source string) ([]Pool, error) {
	if raw == "" {
		return nil, nil
	}
	var pools []Pool
	if err := json.Unmarshal([]byte(raw), &pools); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", source, err)
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
		if p.MaxIdleSeconds < 0 {
			return nil, fmt.Errorf("pool %q: maxIdleSeconds must be non-negative", p.ID)
		}
		for j, v := range p.Volumes {
			if err := v.Validate(fmt.Sprintf("pool %q volumes[%d]", p.ID, j)); err != nil {
				return nil, err
			}
		}
	}
	return pools, nil
}

// ByID indexes a pool list for lookup by the API and the backends.
func ByID(pools []Pool) map[string]*Pool {
	byID := make(map[string]*Pool, len(pools))
	for i := range pools {
		byID[pools[i].ID] = &pools[i]
	}
	return byID
}

// Status is the API view of a configured pool.
type Status struct {
	ID      string `json:"id"`
	Image   string `json:"image"`
	Size    int    `json:"size"`
	Warm    int    `json:"warm"`    // unclaimed, warm-ready pods
	Claimed int    `json:"claimed"` // pods bound to a workload claim
}

// Lister is the part of a warm-pool backend that reports its pools.
type Lister interface {
	Pools(ctx context.Context) ([]Status, error)
}

// StatusFor returns one declared pool's live status. A pool the operator did not
// declare is NotFound, and so is one that is declared but that the backend does
// not report: a caller cannot use either, and telling them apart would leak how
// the backend is configured.
func StatusFor(ctx context.Context, backend Lister, declared map[string]*Pool, id string) (*Status, error) {
	if _, ok := declared[id]; !ok {
		return nil, apperrors.NotFound("pool", id)
	}
	statuses, err := backend.Pools(ctx)
	if err != nil {
		return nil, err
	}
	for i := range statuses {
		if statuses[i].ID == id {
			return &statuses[i], nil
		}
	}
	return nil, apperrors.NotFound("pool", id)
}
