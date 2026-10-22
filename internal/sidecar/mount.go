package sidecar

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/mount"
)

// Mounter mounts a source at a target directory and unmounts it, both given
// relative to the workspace. The source is normally a read-only filesystem
// image (squashfs or erofs); with SourceDir it is a directory materialized from
// a tar archive. With opts.Writable the source becomes the read-only lower
// layer of an overlay. mount.Beneath mounts from this container;
// mountagent.Client has the node mount agent do it.
type Mounter interface {
	Mount(source, target string, opts mount.Opts) error
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
