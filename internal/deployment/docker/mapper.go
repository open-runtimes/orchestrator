package docker

import (
	"encoding/json"
	"net"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
)

// Container labels — the Docker daemon is the deployments store, so the proxy
// container carries the canonical spec and host alongside the shared identity
// labels.
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

// workspacePath is where the shared volume is mounted in every container.
const workspacePath = "/workspace"

// defaultProgressDeadline matches the API default for ProgressDeadlineSeconds.
const defaultProgressDeadline = 600 * time.Second

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

// progressDeadline returns the ready deadline, applying the API default.
func progressDeadline(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultProgressDeadline
	}
	return time.Duration(seconds) * time.Second
}

// specOf returns the canonical spec JSON stored on the proxy container, or ""
// if no proxy container is present.
func specOf(summaries []container.Summary) string {
	for _, c := range summaries {
		if c.Labels[labelType] == typeProxy {
			return c.Labels[labelSpec]
		}
	}
	return ""
}

// specDeadline returns the ready deadline recorded in the proxy's spec label,
// falling back to the API default.
func specDeadline(summaries []container.Summary) time.Duration {
	var req deployment.Request
	if raw := specOf(summaries); raw != "" {
		_ = json.Unmarshal([]byte(raw), &req)
	}
	return progressDeadline(req.ProgressDeadlineSeconds)
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
	proxyRunning   bool
	proxyHealth    container.HealthStatus
	created        time.Time     // proxy creation time (worker's if the proxy is missing)
	deadline       time.Duration // ready deadline from the recorded spec
}

// deriveStatus maps observed container state to a deployment status. Docker
// runs exactly one replica, so desired is always 1.
func deriveStatus(id string, s snapshot, now time.Time) *deployment.StatusResponse {
	status := &deployment.StatusResponse{ID: id, DesiredReplicas: 1}

	switch {
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
