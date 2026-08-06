// Package volume defines persistent storage attached to a workload: an existing
// Docker named volume or Kubernetes PersistentVolumeClaim mounted into the
// worker container. Unlike artifacts, a volume is a spec-time input the
// orchestrator wires into the pod/container — the sidecar never touches it.
package volume

import (
	"orchestrator/internal/apperrors"
	"path"
	"strings"
)

// Volume attaches existing storage to the worker container. Source names an
// existing Docker volume (Docker backend) or PersistentVolumeClaim (K8s
// backend) — volumes are attach-only, never created or sized here.
type Volume struct {
	Source   string `json:"source"`             // existing Docker volume / K8s PVC name
	Path     string `json:"path"`               // absolute mount path in the worker container
	SubPath  string `json:"subPath,omitempty"`  // optional subdirectory within the volume
	ReadOnly bool   `json:"readonly,omitempty"` // mount read-only
}

// Validate checks a volume's fields. field is the caller's path for error
// reporting, e.g. "volumes[0]".
func (v Volume) Validate(field string) error {
	if v.Source == "" {
		return apperrors.Validation(field+".source", "source (volume/PVC name) is required")
	}
	if v.Path == "" {
		return apperrors.Validation(field+".path", "path (mount path) is required")
	}
	if !path.IsAbs(v.Path) {
		return apperrors.Validation(field+".path", "path must be absolute")
	}
	if v.SubPath != "" {
		if path.IsAbs(v.SubPath) {
			return apperrors.Validation(field+".subPath", "subPath must be relative")
		}
		if v.SubPath == ".." || strings.HasPrefix(v.SubPath, "../") ||
			strings.Contains(v.SubPath, "/../") || strings.HasSuffix(v.SubPath, "/..") {
			return apperrors.Validation(field+".subPath", "subPath must not traverse outside the volume")
		}
	}
	return nil
}
