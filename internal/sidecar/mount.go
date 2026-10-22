package sidecar

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/mount"
)

// MountOpts configures a mount (see mount.Opts).
type MountOpts = mount.Opts

// UpperDir is where an overlay's upper layer lives for a mount at target.
func UpperDir(target string) string { return mount.UpperDir(target) }

// Mounter mounts a source at a target directory and unmounts it. The source is
// normally a read-only filesystem image (squashfs or erofs); with SourceDir it
// is a directory materialized from a tar archive. With opts.Writable the source
// becomes the read-only lower layer of a tmpfs-backed overlay. Implementations
// are platform-specific (kernel mounts on Linux).
type Mounter interface {
	Mount(source, target string, opts MountOpts) error
	Unmount(target string) error
	IsMounted(target string) (bool, error)
}

// splitMounts separates mount artifacts from the rest, preserving order.
func splitMounts(arts []artifact.Artifact) (mounts []*artifact.Mount, rest []artifact.Artifact) {
	for _, a := range arts {
		if m, ok := a.(*artifact.Mount); ok {
			mounts = append(mounts, m)
		} else {
			rest = append(rest, a)
		}
	}
	return mounts, rest
}
