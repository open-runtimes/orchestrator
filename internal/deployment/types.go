// Package deployment defines the serving-plane domain: a container spec that
// becomes a long-lived, HTTP-addressable workload. See docs/.
package deployment

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/volume"
)

// Request is the declarative deployment spec. POST is create-or-update.
type Request struct {
	ID           string            `json:"id"` // RFC-1123 label (≤63); part of object names
	Meta         map[string]string `json:"meta,omitempty"`
	Image        string            `json:"image"`
	RuntimeClass string            `json:"runtimeClass,omitempty"` // isolation tier: runc (default) | gvisor | kata (K8s only)
	Command      string            `json:"command,omitempty"`
	CPU          float64           `json:"cpu"`    // limit (cores)
	Memory       int               `json:"memory"` // limit (MB)
	Environment  map[string]string `json:"environment,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`   // working directory and shared-volume mount path (default: /workspace)
	Artifacts    artifact.Set      `json:"artifacts,omitempty"`   // materialized into the workspace before serving
	Volumes      []volume.Volume   `json:"volumes,omitempty"`     // existing Docker volumes / K8s PVCs mounted into the worker
	Hosts        []string          `json:"hosts,omitempty"`       // RFC-1123 hostnames (≤253 each); hosts[0] is the primary; empty = [{id}.{domain}]
	Port         int               `json:"port"`                  // container port serving HTTP
	Replicas     int               `json:"replicas,omitempty"`    // fixed count; default 1 (Docker: always 1)
	Concurrency  int               `json:"concurrency,omitempty"` // hard per-replica in-flight cap; 0 = unlimited
	Autoscaling  *Autoscaling      `json:"autoscaling,omitempty"`
	Probes       *Probes           `json:"probes,omitempty"`
	Callback     *Callback         `json:"callback,omitempty"`

	TimeoutSeconds      int `json:"timeoutSeconds,omitempty"`      // per-request total → 504; default 300
	StartTimeoutSeconds int `json:"startTimeoutSeconds,omitempty"` // activator wait for a ready endpoint → 503; default 300
	ReadyTimeoutSeconds int `json:"readyTimeoutSeconds,omitempty"` // ready deadline → failed; default 600
	// TerminationGracePeriodSeconds is part of the fixed pod shape used for
	// transparent warm-pool matching. Zero means the Kubernetes default (30s).
	TerminationGracePeriodSeconds int `json:"terminationGracePeriodSeconds,omitempty"`
}

// Probes — only Readiness is sidecar-run (honors ms granularity); Liveness and
// Startup are kubelet-run at whole-second granularity (ms rounded up, 1s min).
type Probes struct {
	Readiness *Probe `json:"readiness,omitempty"` // sidecar-run; gates traffic; sub-second
	Liveness  *Probe `json:"liveness,omitempty"`  // kubelet-run; restarts the container; ≥1s
	Startup   *Probe `json:"startup,omitempty"`   // kubelet-run; slow-boot grace; ≥1s
}

// Probe mirrors the k8s Probe shape. Millisecond fields are honored sub-second
// only for the sidecar-run readiness probe.
type Probe struct {
	Path             string `json:"path,omitempty"`             // HTTP GET path on Port; empty = TCP connect
	PeriodMillis     int    `json:"periodMillis,omitempty"`     // k8s periodSeconds
	TimeoutMillis    int    `json:"timeoutMillis,omitempty"`    // k8s timeoutSeconds
	FailureThreshold int    `json:"failureThreshold,omitempty"` // give-up = threshold × period
}

// Autoscaling opts a deployment into concurrency-managed replicas:
// desired = clamp(ceil(avgConcurrency / target), minReplicas, maxReplicas),
// averaged over the operator's window. minReplicas: 0 enables scale-to-zero;
// the activator owns the 0→N raise on a cold hit. Docker honors only 0↔1.
type Autoscaling struct {
	MinReplicas int `json:"minReplicas"`           // 0 = scale-to-zero
	MaxReplicas int `json:"maxReplicas,omitempty"` // default max(replicas, 1)
	Target      int `json:"target,omitempty"`      // in-flight per replica driving scaling; default 100
}

// Callback is where async responses (and, later, lifecycle events) are delivered.
type Callback struct {
	URL    string   `json:"url"`
	Events []string `json:"events,omitempty"`
	Key    string   `json:"key,omitempty"` // HMAC signing key
}

// Isolation tiers (docs/operations.md): the RuntimeClass
// for the workload pod. Kubernetes only; the empty string means runc.
const (
	RuntimeClassRunc   = "runc" // default: shared host kernel, hardening floor only
	RuntimeClassGvisor = "gvisor"
	RuntimeClassKata   = "kata"
)

// ValidRuntimeClass reports whether s names an isolation tier ("" = runc default).
func ValidRuntimeClass(s string) bool {
	return s == "" || s == RuntimeClassRunc || s == RuntimeClassGvisor || s == RuntimeClassKata
}

// Deployment states.
const (
	StatePending  = "pending"
	StateReady    = "ready"
	StateIdle     = "idle" // scaled to zero; the next request cold-starts it
	StateDegraded = "degraded"
	StateFailed   = "failed"
	StateDeleting = "deleting"
)

// Target is one entry in a deployment's traffic table: a revision and its
// share of requests. Percents across a table sum to 100.
type Target struct {
	RevisionName string `json:"revisionName"`
	Percent      int    `json:"percent"`
}

// Traffic modes: auto follows the latest ready revision (auto-cut); manual
// pins the operator's traffic table until released.
const (
	ModeAuto   = "auto"
	ModeManual = "manual"
)

// StatusResponse is the API view of a deployment.
type StatusResponse struct {
	ID                string   `json:"id"`
	State             string   `json:"status"`              // pending|ready|idle|degraded|failed|deleting
	URL               string   `json:"url"`                 // gateway URL (K8s) / activator URL (Docker)
	Revisions         []string `json:"revisions,omitempty"` // newest first; empty on Docker (single-revision)
	Traffic           []Target `json:"traffic,omitempty"`
	Mode              string   `json:"mode,omitempty"` // auto|manual — whether new revisions auto-cut
	DesiredReplicas   int      `json:"desiredReplicas"`
	AvailableReplicas int      `json:"availableReplicas"`
	Error             string   `json:"error,omitempty"`
}

// ListResponse is the response for listing deployments.
type ListResponse struct {
	Deployments []StatusResponse `json:"deployments"`
}

// Parse decodes an API request body, rejecting unknown fields — a typo'd
// field name must fail loudly, not silently deploy defaults. Strictness
// belongs at the API edge only: stored specs (Spec Secrets, volume labels)
// decode leniently so version skew never strands them.
func Parse(data []byte) (*Request, error) {
	var r Request
	if err := artifact.UnmarshalStrict(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
