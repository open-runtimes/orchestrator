// Package mount holds the kernel side of artifact mounts: loop devices, image
// filesystem detection, the on-disk layout of an overlay, and mountinfo
// parsing. Two callers share it: the in-pod kernel mounter the sidecar uses
// when it runs privileged, and the node mount agent, which performs the same
// mounts from outside the pod through a Root confined to the pod's workspace.
package mount

import "path/filepath"

// Opts configures a mount. The zero value is a plain read-only image mount;
// Writable layers an overlay on top, and SizeMiB caps it (0 = kernel default).
type Opts struct {
	Writable bool
	SizeMiB  int
	// SourceDir says the source is an already-materialized directory rather
	// than a loop-mountable filesystem image. Tar archives are extracted into
	// a lower directory inside the future mount point, so the resulting bind or
	// overlay hides that implementation path from the worker.
	SourceDir bool
	// UpperOnDisk puts the overlay's upper layer on the shared workspace instead
	// of a tmpfs. A tmpfs upper is RAM and dies with the pod, which is the right
	// default; a synced mount needs the delta to be an ordinary directory that
	// outlives each write and can be read by the artifact runner. SizeMiB does
	// not apply — the workspace volume's own limit does.
	UpperOnDisk bool
}

// LowerDir lives inside the directory that is about to become the mount point.
// Once the bind or overlay is established at target, the implementation lower
// is hidden from the worker; unmounting target reveals it for cleanup.
func LowerDir(target string) string { return filepath.Join(target, ".lower") }

// ScratchDir holds an overlay's upper and work layers. It stays a sibling of
// target because a writable overlay must keep its upper accessible to the
// artifact runner for sync.
func ScratchDir(target string) string { return target + ".scratch" }

// UpperDir is where an overlay's upper layer lives for a mount at target: the
// delta, and nothing else. Everything untouched stays in the image, so this is
// exactly what a sync has to carry.
func UpperDir(target string) string { return filepath.Join(ScratchDir(target), "upper") }

// WorkDir is overlayfs's private work directory, beside the upper on the same
// filesystem as overlayfs requires.
func WorkDir(target string) string { return filepath.Join(ScratchDir(target), "work") }
