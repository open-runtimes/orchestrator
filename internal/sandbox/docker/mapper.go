package docker

import (
	"encoding/json"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/config"
	"orchestrator/internal/sandbox"
	"strconv"
	"strings"
)

// Labels — the Docker daemon is the sandbox store. The workspace volume is the
// identity anchor: it carries the sandbox id, its spec, and its capability
// token, so listing volumes reconstructs every sandbox after a restart. The
// containers carry the same id label so they can be found and reaped.
const (
	labelManagedBy = "managed-by"
	labelID        = "sandbox.id"
	labelType      = "sandbox.type"
	labelSpec      = "sandbox.spec"
	// labelToken carries the capability token the edge routes by. It is the
	// credential, so it lives on the sandbox's own objects and dies with them.
	labelToken = "sandbox.token"
	labelPool  = "sandbox.pool"

	managedByValue = "sandbox-service"

	typeWorker    = "worker"
	typeProxy     = "proxy"
	typeArtifacts = "artifacts"
	typeAgent     = "agent-install"
)

// workspacePath is where the workspace volume mounts in every container of a
// sandbox — the sandbox image's own default, and the shim's on Kubernetes.
const workspacePath = config.DefaultWorkspace

func workerName(id string) string    { return "sbx-" + id + "-worker" }
func proxyName(id string) string     { return "sbx-" + id + "-proxy" }
func artifactsName(id string) string { return "sbx-" + id + "-artifacts" }
func agentName(id string) string     { return "sbx-" + id + "-agent" }
func volumeName(id string) string    { return "sbx-" + id + "-workspace" }

// containerLabels returns the base labels for one of a sandbox's containers.
func containerLabels(id, containerType string) map[string]string {
	return map[string]string{
		labelManagedBy: managedByValue,
		labelID:        id,
		labelType:      containerType,
	}
}

// volumeLabels returns the labels for a sandbox's workspace volume — the
// authoritative record of what the sandbox is.
func volumeLabels(req *sandbox.Request, spec string) map[string]string {
	return map[string]string{
		labelManagedBy: managedByValue,
		labelID:        req.ID,
		labelPool:      req.Pool,
		labelToken:     req.Token,
		labelSpec:      spec,
	}
}

// parseSpec decodes the spec JSON stored on the volume's label.
func parseSpec(raw string) (*sandbox.Request, error) {
	req := &sandbox.Request{}
	if err := json.Unmarshal([]byte(raw), req); err != nil {
		return nil, apperrors.Internal("docker.unmarshalSpec", err)
	}
	return req, nil
}

// portList renders extra ports for the sidecar's PROXY_EXTRA_PORTS.
func portList(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		out = append(out, strconv.Itoa(port))
	}
	return strings.Join(out, ",")
}
