package docker

import (
	"encoding/json"
	"net"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/config"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// Labels — the Docker daemon is the deployments store. The workspace volume is
// the identity anchor and carries the canonical spec and host, so a deployment
// exists (possibly idle, with no containers) iff its volume does. The proxy
// container mirrors the spec and host labels for observability only.
const (
	labelManagedBy = "managed-by"
	labelID        = "deployment.id"
	labelType      = "deployment.type"
	labelSpec      = "deployment.spec"
	labelHost      = "deployment.host"

	managedByValue = "deployments-service"

	typeWorker    = "worker"
	typeProxy     = "proxy"
	typeArtifacts = "artifacts"
)

// workspacePath is the default shared-volume mount path when a request does
// not set req.Workspace.
const workspacePath = config.DefaultWorkspace

// workspaceOf is the request's workspace (working directory and shared-volume
// mount path), falling back to the default for specs stored before the field
// existed. Every container in a deployment must agree on it.
func workspaceOf(req *deployment.Request) string {
	if req.Workspace != "" {
		return req.Workspace
	}
	return workspacePath
}

// defaultReadyTimeout matches the API default for ReadyTimeoutSeconds.
const defaultReadyTimeout = 600 * time.Second

func workerName(id string) string    { return "dep-" + id + "-worker" }
func proxyName(id string) string     { return "dep-" + id + "-proxy" }
func artifactsName(id string) string { return "dep-" + id + "-artifacts" }
func volumeName(id string) string    { return "dep-" + id + "-workspace" }

// containerLabels returns the base labels for a deployment container.
func containerLabels(id, containerType string) map[string]string {
	return map[string]string{
		labelManagedBy: managedByValue,
		labelID:        id,
		labelType:      containerType,
	}
}

// volumeLabels returns the labels for a deployment's workspace volume — the
// authoritative home of the canonical spec and host.
func volumeLabels(req *deployment.Request, spec string) map[string]string {
	return map[string]string{
		labelManagedBy: managedByValue,
		labelID:        req.ID,
		labelSpec:      spec,
		labelHost:      strings.Join(req.Hosts, ","),
	}
}

// readyTimeout returns the ready deadline, applying the API default.
func readyTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultReadyTimeout
	}
	return time.Duration(seconds) * time.Second
}

// parseSpec decodes the canonical spec JSON stored on the volume's label.
func parseSpec(raw string) (*deployment.Request, error) {
	req := &deployment.Request{}
	if err := json.Unmarshal([]byte(raw), req); err != nil {
		return nil, apperrors.Internal("docker.unmarshalSpec", err)
	}
	return req, nil
}

// specDeadline returns the ready deadline recorded in the spec JSON, falling
// back to the API default.
func specDeadline(raw string) time.Duration {
	var req deployment.Request
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &req)
	}
	return readyTimeout(req.ReadyTimeoutSeconds)
}

// containerIP returns the container's address on the configured network, or
// on Docker's default bridge when no network is configured.
func containerIP(ns *container.NetworkSettings, networkName string) string {
	if ns == nil {
		return ""
	}
	if networkName == "" {
		networkName = "bridge" // Docker's default network
	}
	if ep := ns.Networks[networkName]; ep != nil {
		return ep.IPAddress
	}
	return ""
}

// proxyEnv builds the proxy container environment per the internal/proxy
// contract. Zero-valued knobs are omitted so the proxy's own defaults apply.
func proxyEnv(req *deployment.Request, workerIP string) []string {
	env := []string{proxy.EnvTarget + "=" + net.JoinHostPort(workerIP, strconv.Itoa(req.Port))}
	if req.TimeoutSeconds > 0 {
		env = append(env, proxy.EnvTimeoutSeconds+"="+strconv.Itoa(req.TimeoutSeconds))
	}
	if req.Concurrency > 0 {
		env = append(env, proxy.EnvConcurrency+"="+strconv.Itoa(req.Concurrency))
	}
	if req.Probes == nil || req.Probes.Readiness == nil {
		return env
	}
	r := req.Probes.Readiness
	if r.Path != "" {
		env = append(env, proxy.EnvReadinessPath+"="+r.Path)
	}
	if r.PeriodMillis > 0 {
		env = append(env, proxy.EnvReadinessPeriodMillis+"="+strconv.Itoa(r.PeriodMillis))
	}
	if r.TimeoutMillis > 0 {
		env = append(env, proxy.EnvReadinessTimeoutMillis+"="+strconv.Itoa(r.TimeoutMillis))
	}
	if r.FailureThreshold > 0 {
		env = append(env, proxy.EnvReadinessFailureThreshold+"="+strconv.Itoa(r.FailureThreshold))
	}
	return env
}

// snapshot is the observed Docker state for one deployment, gathered from
// container inspects and reduced to what status derivation needs.
type snapshot struct {
	workerExists   bool
	workerRunning  bool
	workerExitCode int
	proxyExists    bool
	proxyRunning   bool
	proxyHealth    container.HealthStatus
	created        time.Time     // proxy creation time (worker's if the proxy is missing)
	deadline       time.Duration // ready deadline from the recorded spec
}

// deriveStatus maps observed container state to a deployment status. Docker
// runs at most one replica: no containers means scaled to zero (idle),
// otherwise desired is 1.
func deriveStatus(id string, s snapshot, now time.Time) *deployment.StatusResponse {
	status := &deployment.StatusResponse{ID: id, DesiredReplicas: 1, Mode: deployment.ModeAuto}

	switch {
	case !s.workerExists && !s.proxyExists:
		status.State = deployment.StateIdle
		status.DesiredReplicas = 0
	case s.workerRunning && s.proxyRunning && s.proxyHealth == container.Healthy:
		status.State = deployment.StateReady
		status.AvailableReplicas = 1
	case s.workerExists && !s.workerRunning:
		status.State = deployment.StateFailed
		status.Error = "worker exited with code " + strconv.Itoa(s.workerExitCode)
	case now.Sub(s.created) <= s.deadline:
		status.State = deployment.StatePending
	default:
		status.State = deployment.StateFailed
		status.Error = "not ready within " + s.deadline.String()
		if s.proxyHealth != "" {
			status.Error += " (proxy health: " + s.proxyHealth + ")"
		}
	}

	return status
}
