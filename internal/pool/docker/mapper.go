package docker

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"orchestrator/pkg/pool"

	"github.com/docker/docker/api/types/network"
)

// Labels — the Docker daemon is the pool store. Every slot container and its
// workspace volume carry the pool and slot labels; the sidecar additionally
// carries the claim token so the service can re-read it statelessly (dev
// backend — the token never leaves the local daemon). Claim state itself
// lives in the sidecar (labels are immutable): GET /activation distinguishes
// warm, claimed, and poisoned.
const (
	labelManagedBy  = "managed-by"
	labelPoolID     = "pool.id"
	labelSlot       = "pool.slot"
	labelType       = "pool.type"
	labelClaimToken = "pool.claim-token"

	managedByValue = "deployments-service"

	typeSidecar  = "sidecar"
	typeWorkload = "workload"
	typeInstall  = "install"
)

const (
	// workspacePath is where the shared volume is mounted in every container.
	workspacePath = "/workspace"
	// shimPath is where shim-install drops the pool-shim binary — the
	// workload container's entrypoint.
	shimPath = workspacePath + "/.pool/shim"
)

// maxOutputBytes caps an exec activation's captured output (design default).
const maxOutputBytes = 1 << 20

// A slot is the Docker equivalent of a warm pod: sidecar + workload sharing a
// workspace volume, all named by pool ID and a random slot suffix.
func slotPrefix(poolID, slot string) string   { return "pool-" + poolID + "-" + slot }
func volumeName(poolID, slot string) string   { return slotPrefix(poolID, slot) + "-ws" }
func sidecarName(poolID, slot string) string  { return slotPrefix(poolID, slot) + "-sidecar" }
func workloadName(poolID, slot string) string { return slotPrefix(poolID, slot) + "-workload" }
func installName(poolID, slot string) string  { return slotPrefix(poolID, slot) + "-shim" }

// slotLabels returns the labels for a slot's containers and volume.
func slotLabels(poolID, slot, containerType string) map[string]string {
	labels := map[string]string{
		labelManagedBy: managedByValue,
		labelPoolID:    poolID,
		labelSlot:      slot,
	}
	if containerType != "" {
		labels[labelType] = containerType
	}
	return labels
}

// randomHex returns n random bytes hex-encoded (2n characters).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// activationState maps the observed workload container to an activation
// state: the workload runs the payload once claimed, so running → ready, a
// stop → exited, and a missing container → failed (removed out-of-band, e.g.
// by the activation timeout).
func activationState(exists, running bool) string {
	switch {
	case !exists:
		return pool.StateFailed
	case running:
		return pool.StateReady
	default:
		return pool.StateExited
	}
}

// containerIP returns the container's address on the configured network, or
// on Docker's default bridge when no network is configured.
func containerIP(networks map[string]*network.EndpointSettings, networkName string) string {
	if networkName == "" {
		networkName = "bridge" // Docker's default network
	}
	if ep := networks[networkName]; ep != nil {
		return ep.IPAddress
	}
	return ""
}

// cappedWriter keeps the first cap bytes written and flags truncation, per
// the design's bounded-Output contract.
type cappedWriter struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.cap - w.buf.Len(); room < len(p) {
		p = p[:room]
		w.truncated = true
	}
	w.buf.Write(p)
	return n, nil
}

func (w *cappedWriter) String() string {
	if w.truncated {
		return w.buf.String() + "\n[output truncated]"
	}
	return w.buf.String()
}
