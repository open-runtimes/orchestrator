package sidecar

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A marker is the sidecar's crash-safe record that a phase completed. Markers
// live in the shared workspace because that is the only state that survives a
// container restart: a restarted sidecar reads them — alongside the kernel
// mount table for mount adoption — to recover in place instead of redoing
// work a running workload may depend on.
//
// Every marker lives under markerDir, a runner-owned corner of the workspace,
// so an artifact's out path can never collide with runner state. Nothing
// except the ready marker is consumed externally. ReadyMarkerPath is the
// startup contract shared with the Kubernetes worker shell gate.
type marker string

const (
	// markerDir is the runner-owned directory inside the shared workspace.
	markerDir = ".sidecar"

	// markerReady gates the worker in the combined flow: pre-job artifacts
	// are processed and mounts are established. Docker health checks and
	// Kubernetes worker shell gates poll it.
	markerReady marker = "ready"

	// markerMountsReady gates the worker in the split flow: the post sidecar
	// has established every artifact mount. Polled via -check-mounts.
	markerMountsReady marker = "mounts-ready"

	// markerArtifactsComplete records that the pre-job artifact phase
	// finished. A restarted sidecar skips the phase instead of re-fetching
	// files a running workload may be mounted on.
	markerArtifactsComplete marker = "artifacts-complete"
)

// path of m inside workspace.
func (m marker) path(workspace string) string {
	return filepath.Join(workspace, markerDir, string(m))
}

// exists reports whether m is set in workspace.
func (m marker) exists(workspace string) bool {
	_, err := os.Stat(m.path(workspace))
	return err == nil
}

// write sets m in workspace.
func (m marker) write(workspace string) error {
	path := m.path(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create marker directory: %w", err)
	}
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		return fmt.Errorf("write %s marker: %w", m, err)
	}
	return nil
}

// clear removes m from workspace; a marker that was never set is fine.
func (m marker) clear(workspace string) error {
	if err := os.Remove(m.path(workspace)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear %s marker: %w", m, err)
	}
	return nil
}

// ReadyMarkerPath is where the ready marker lives inside workspace. The
// worker shell gate consumes this path as part of the startup contract.
func ReadyMarkerPath(workspace string) string {
	return markerReady.path(workspace)
}
