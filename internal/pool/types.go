// Package pool defines the warm-pool domain: config-declared pools of a
// runtime image kept idle, onto which a claim late-binds a payload — claim +
// inject + exec instead of schedule + pull + start. The Pool declaration is
// shared by every consumer of standing warm capacity (deployment Revisions
// and sandboxes). See docs/pools.md.
package pool

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"orchestrator/internal/claim"
	"orchestrator/internal/deployment"
	"orchestrator/internal/volume"
	"slices"
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
	// Command is retained for the shared claim shape, but transparent pools
	// reject it: command is always late-bound from the workload request.
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

	// Burst controls acquisition when no warm pod is free: "cold" (default)
	// creates through the pool; "reject" declines the warm optimization and
	// lets the consumer use its retained direct template. Always logged.
	Burst string `json:"burst,omitempty"`

	// MaxIdleSeconds is retained so older config fails with an actionable error.
	// Transparent pools cannot alter request-time idle policy and reject it.
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

// ShapeKey returns a stable identity for the fields fixed when a pod is
// created. Request-time fields and capacity policy are deliberately excluded.
func ShapeKey(shape *Spec) string {
	runtimeClass := shape.RuntimeClass
	if runtimeClass == "" {
		runtimeClass = deployment.RuntimeClassRunc
	}
	grace := shape.TerminationGracePeriodSeconds
	if grace == 0 {
		grace = 30
	}
	volumes := append([]volume.Volume(nil), shape.Volumes...)
	slices.SortFunc(volumes, func(a, b volume.Volume) int {
		if n := cmp.Compare(a.Source, b.Source); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Path, b.Path); n != 0 {
			return n
		}
		if n := cmp.Compare(a.SubPath, b.SubPath); n != 0 {
			return n
		}
		if a.ReadOnly == b.ReadOnly {
			return 0
		}
		if !a.ReadOnly {
			return -1
		}
		return 1
	})
	canonical := struct {
		Image        string          `json:"image"`
		Port         int             `json:"port"`
		CPU          float64         `json:"cpu"`
		Memory       int             `json:"memory"`
		RuntimeClass string          `json:"runtimeClass"`
		Volumes      []volume.Volume `json:"volumes,omitempty"`
		Mounts       bool            `json:"mounts,omitempty"`
		Grace        int             `json:"terminationGracePeriodSeconds"`
	}{shape.Image, shape.Port, shape.CPU, shape.Memory, runtimeClass, volumes, shape.Mounts, grace}
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum[:])
}

// Match returns the configured pool whose fixed pod shape exactly equals the
// requested shape. Pool validation ensures at most one can match.
func Match(pools []Pool, shape *Spec) *Pool {
	key := ShapeKey(shape)
	for i := range pools {
		if ShapeKey(&pools[i].Spec) == key {
			return &pools[i]
		}
	}
	return nil
}

// ValidateTransparent checks the invariants required for implicit exact-shape
// selection shared by Revision and sandbox pools.
func ValidateTransparent(pools []Pool, kind string) error {
	for i := range pools {
		p := &pools[i]
		if p.CPU <= 0 || p.Memory <= 0 {
			return fmt.Errorf("%s pool %q: cpu and memory are required for exact shape matching", kind, p.ID)
		}
		if p.Command != "" || len(p.Environment) != 0 {
			return fmt.Errorf("%s pool %q: command and environment are request-time fields and must not be configured on the pool", kind, p.ID)
		}
		if p.MaxIdleSeconds != 0 {
			return fmt.Errorf("%s pool %q: maxIdleSeconds is a request-time policy and must not be configured on the pool", kind, p.ID)
		}
		for j := range i {
			if ShapeKey(&pools[j].Spec) == ShapeKey(&p.Spec) {
				return fmt.Errorf("%s pools %q and %q declare the same fixed shape", kind, pools[j].ID, p.ID)
			}
		}
	}
	return nil
}

// Status is the API view of a configured pool.
type Status struct {
	ID      string `json:"id"`
	Image   string `json:"image"`
	Size    int    `json:"size"`
	Warm    int    `json:"warm"`    // unclaimed, warm-ready pods
	Claimed int    `json:"claimed"` // pods bound to a workload claim
}
