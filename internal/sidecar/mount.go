package sidecar

import (
	"orchestrator/internal/artifact"
	"os"
	"path/filepath"
)

// MountReadyFile marks that all image mounts are established. The Kubernetes
// native sidecar writes it after mounting; the worker's startup probe waits on
// it so the worker only starts once its mounts are present.
const MountReadyFile = ".mounts-ready"

// MountOpts configures a mount. The zero value is a plain read-only image
// mount; Writable layers an overlay on top, and SizeMiB caps it (0 = kernel
// default).
type MountOpts struct {
	Writable bool
	SizeMiB  int
	// UpperOnDisk puts the overlay's upper layer on the shared workspace instead
	// of a tmpfs. A tmpfs upper is RAM and dies with the pod, which is the right
	// default; a synced mount needs the delta to be an ordinary directory that
	// outlives each write and can be read by the artifact runner. SizeMiB does
	// not apply — the workspace volume's own limit does.
	UpperOnDisk bool
}

// UpperDir is where an overlay's upper layer lives for a mount at target: the
// delta, and nothing else. Everything untouched stays in the image, so this is
// exactly what a sync has to carry.
func UpperDir(target string) string { return filepath.Join(target+".scratch", "upper") }

// Mounter mounts a read-only filesystem image (squashfs or erofs) at a target
// directory and unmounts it. With opts.Writable the image becomes the read-only
// lower layer of a tmpfs-backed overlay. Implementations are platform-specific
// (kernel loop mount on Linux).
type Mounter interface {
	Mount(image, target string, opts MountOpts) error
	Unmount(target string) error
}

// CheckMountsReady reports whether the mounts-ready marker exists.
func CheckMountsReady(sharedVolumePath string) bool {
	_, err := os.Stat(filepath.Join(sharedVolumePath, MountReadyFile))
	return err == nil
}

// splitMounts separates mount artifacts from the rest, preserving order.
func splitMounts(arts []artifact.Artifact) (mounts, rest []artifact.Artifact) {
	for _, a := range arts {
		if a.ArtifactType() == "mount" {
			mounts = append(mounts, a)
		} else {
			rest = append(rest, a)
		}
	}
	return mounts, rest
}
