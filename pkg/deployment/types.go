// Package deployment defines the serving-plane domain: a container spec that
// becomes a long-lived, HTTP-addressable workload. See docs/design.
package deployment

import (
	"encoding/json"
	"fmt"
	"orchestrator/internal/artifact"
)

// Request is the declarative deployment spec. POST is create-or-update.
type Request struct {
	ID          string              `json:"id"` // RFC-1123 label (≤63); part of object names
	Meta        map[string]string   `json:"meta,omitempty"`
	Image       string              `json:"image"`
	Command     string              `json:"command,omitempty"`
	CPU         float64             `json:"cpu"`    // limit (cores)
	Memory      int                 `json:"memory"` // limit (MB)
	Environment map[string]string   `json:"environment,omitempty"`
	Artifacts   []artifact.Artifact `json:"artifacts,omitempty"`   // materialized into the workspace before serving
	Host        string              `json:"host,omitempty"`        // RFC-1123 hostname (≤253); else {id}.{domain}
	Port        int                 `json:"port"`                  // container port serving HTTP
	Replicas    int                 `json:"replicas,omitempty"`    // fixed count; default 1 (Docker: always 1)
	Concurrency int                 `json:"concurrency,omitempty"` // hard per-replica in-flight cap; 0 = unlimited
	Autoscaling *Autoscaling        `json:"autoscaling,omitempty"`
	Probes      *Probes             `json:"probes,omitempty"`
	Callback    *Callback           `json:"callback,omitempty"`

	TimeoutSeconds              int `json:"timeoutSeconds,omitempty"`              // per-request total → 504; default 300
	ResponseStartTimeoutSeconds int `json:"responseStartTimeoutSeconds,omitempty"` // activator wait for a ready endpoint → 503; default 300
	ProgressDeadlineSeconds     int `json:"progressDeadlineSeconds,omitempty"`     // ready deadline → failed; default 600
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

// Autoscaling opts a deployment into managed replicas. minReplicas: 0 enables
// scale-to-zero — idle past the operator's window scales down, the next
// request cold-starts it back to `replicas`. Concurrency-driven 1↔N
// (maxReplicas/target) arrives in Phase 4.
type Autoscaling struct {
	MinReplicas int `json:"minReplicas"` // 0 = scale-to-zero
}

// Callback is where async responses (and, later, lifecycle events) are delivered.
type Callback struct {
	URL    string   `json:"url"`
	Events []string `json:"events,omitempty"`
	Key    string   `json:"key,omitempty"` // HMAC signing key
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

// StatusResponse is the API view of a deployment.
type StatusResponse struct {
	ID                string `json:"id"`
	State             string `json:"status"` // pending|ready|degraded|failed|deleting
	URL               string `json:"url"`    // activator URL (Phase 1) / gateway URL (Phase 3+)
	DesiredReplicas   int    `json:"desiredReplicas"`
	AvailableReplicas int    `json:"availableReplicas"`
	Error             string `json:"error,omitempty"`
}

// ListResponse is the response for listing deployments.
type ListResponse struct {
	Deployments []StatusResponse `json:"deployments"`
}

// requestJSON mirrors Request with json.RawMessage artifacts (same pattern as pkg/job).
type requestJSON struct {
	ID          string            `json:"id"`
	Meta        map[string]string `json:"meta,omitempty"`
	Image       string            `json:"image"`
	Command     string            `json:"command,omitempty"`
	CPU         float64           `json:"cpu"`
	Memory      int               `json:"memory"`
	Environment map[string]string `json:"environment,omitempty"`
	Artifacts   json.RawMessage   `json:"artifacts,omitempty"`
	Host        string            `json:"host,omitempty"`
	Port        int               `json:"port"`
	Replicas    int               `json:"replicas,omitempty"`
	Concurrency int               `json:"concurrency,omitempty"`
	Autoscaling *Autoscaling      `json:"autoscaling,omitempty"`
	Probes      *Probes           `json:"probes,omitempty"`
	Callback    *Callback         `json:"callback,omitempty"`

	TimeoutSeconds              int `json:"timeoutSeconds,omitempty"`
	ResponseStartTimeoutSeconds int `json:"responseStartTimeoutSeconds,omitempty"`
	ProgressDeadlineSeconds     int `json:"progressDeadlineSeconds,omitempty"`
}

// UnmarshalJSON decodes artifacts into their concrete types via the registry.
func (r *Request) UnmarshalJSON(data []byte) error {
	var raw requestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	r.ID = raw.ID
	r.Meta = raw.Meta
	r.Image = raw.Image
	r.Command = raw.Command
	r.CPU = raw.CPU
	r.Memory = raw.Memory
	r.Environment = raw.Environment
	r.Host = raw.Host
	r.Port = raw.Port
	r.Replicas = raw.Replicas
	r.Concurrency = raw.Concurrency
	r.Autoscaling = raw.Autoscaling
	r.Probes = raw.Probes
	r.Callback = raw.Callback
	r.TimeoutSeconds = raw.TimeoutSeconds
	r.ResponseStartTimeoutSeconds = raw.ResponseStartTimeoutSeconds
	r.ProgressDeadlineSeconds = raw.ProgressDeadlineSeconds

	if len(raw.Artifacts) > 0 && string(raw.Artifacts) != "null" {
		artifacts, err := artifact.UnmarshalArtifacts(raw.Artifacts)
		if err != nil {
			return fmt.Errorf("failed to unmarshal artifacts: %w", err)
		}
		r.Artifacts = artifacts
	}

	return nil
}

// MarshalJSON encodes artifacts with their type discriminator.
func (r Request) MarshalJSON() ([]byte, error) {
	raw := requestJSON{
		ID:          r.ID,
		Meta:        r.Meta,
		Image:       r.Image,
		Command:     r.Command,
		CPU:         r.CPU,
		Memory:      r.Memory,
		Environment: r.Environment,
		Host:        r.Host,
		Port:        r.Port,
		Replicas:    r.Replicas,
		Concurrency: r.Concurrency,
		Autoscaling: r.Autoscaling,
		Probes:      r.Probes,
		Callback:    r.Callback,

		TimeoutSeconds:              r.TimeoutSeconds,
		ResponseStartTimeoutSeconds: r.ResponseStartTimeoutSeconds,
		ProgressDeadlineSeconds:     r.ProgressDeadlineSeconds,
	}

	if len(r.Artifacts) > 0 {
		artifactsData, err := artifact.MarshalArtifacts(r.Artifacts)
		if err != nil {
			return nil, err
		}
		raw.Artifacts = artifactsData
	}

	return json.Marshal(raw)
}
