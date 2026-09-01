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
// so an artifact's out path can never collide with runner state. Probes never
// read the paths — they exec the -check-* flags. The one marker whose path is
// public is ready (see ReadyMarkerPath): workloads that gate themselves
// in-process wait on it — e.g. an app container started in parallel with this
// sidecar, spin-waiting at tens of milliseconds instead of sitting behind a
// whole-second kubelet startup probe. That path is a stable contract; the pod
// composer ships this binary and the workload's wait together, so moving it
// is a breaking change.
type marker string

const (
	// markerDir is the runner-owned directory inside the shared workspace.
	markerDir = ".sidecar"

	// markerReady gates the worker in the combined flow: pre-job artifacts
	// are processed and mounts are established. Docker health checks and
	// Kubernetes startup probes poll it via -check-ready.
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

// ReadyMarkerPath is where the ready marker lives inside workspace: the
// stable, workload-facing gate. It is written only once artifacts are
// processed and mounts established, and cleared before an incarnation
// (re)runs setup — so a workload booting during sidecar recovery only ever
// starts behind re-established mounts.
func ReadyMarkerPath(workspace string) string {
	return markerReady.path(workspace)
}
