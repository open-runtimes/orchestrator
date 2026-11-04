//go:build linux

package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"orchestrator/internal/mount"
	"os"
	"path/filepath"
)

// kubeletUserns is the mapping kubelet records for a `hostUsers: false` pod,
// in <pod dir>/userns, before it creates the pod's sandbox
// (pkg/kubelet/userns/userns_manager.go).
type kubeletUserns struct {
	UIDMappings []kubeletIDMapping `json:"uidMappings"`
	GIDMappings []kubeletIDMapping `json:"gidMappings"`
}

type kubeletIDMapping struct {
	HostID      uint32 `json:"hostId"`
	ContainerID uint32 `json:"containerId"`
	Length      uint32 `json:"length"`
}

// podIDMap returns the user namespace of the pod whose workspace kubelet
// published at target, or nil if the pod shares the host's. The mapping comes
// from kubelet's record rather than from a process in the pod, so nothing the
// workload controls decides it. target is
// <kubelet>/pods/<uid>/volumes/kubernetes.io~csi/<volume>/mount, and kubelet
// writes the record before the pod's containers exist, so by the time its
// sidecar asks for a mount the record is there or the pod has none.
func podIDMap(target string) (*mount.IDMap, error) {
	csiDir := filepath.Dir(filepath.Dir(target))
	if filepath.Base(csiDir) != "kubernetes.io~csi" {
		return nil, nil //nolint:nilnil // nil is the host's user namespace; not kubelet's layout (the agent's tests)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(csiDir)), "userns"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // no record: the pod shares the host's user namespace
	}
	if err != nil {
		return nil, fmt.Errorf("read the pod's user namespace: %w", err)
	}
	var ns kubeletUserns
	if err := json.Unmarshal(data, &ns); err != nil {
		return nil, fmt.Errorf("parse the pod's user namespace: %w", err)
	}
	return mount.NewIDMap(idRanges(ns.UIDMappings), idRanges(ns.GIDMappings))
}

func idRanges(in []kubeletIDMapping) []mount.IDRange {
	out := make([]mount.IDRange, len(in))
	for i, m := range in {
		out[i] = mount.IDRange{ContainerID: m.ContainerID, HostID: m.HostID, Length: m.Length}
	}
	return out
}
