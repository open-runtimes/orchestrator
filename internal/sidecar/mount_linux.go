//go:build linux

package sidecar

import (
	"fmt"
	"orchestrator/internal/mount"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// kernelMounter mounts read-only filesystem images (squashfs or erofs) via a
// loop device, or a materialized tar directory via a bind mount. It uses only
// mount(2), so it works in a distroless image. It needs CAP_SYS_ADMIN and, for
// images, access to loop devices and the matching filesystem kernel module.
type kernelMounter struct{}

//nolint:iface // platform builds return different concrete Mounter types
func defaultMounter() Mounter { return kernelMounter{} }

// Mount mounts image at target. A read-only mount loop-mounts the image
// directly. A writable mount makes that read-only image the lower layer of an
// overlay whose upper/work layers live on a fresh tmpfs — the classic
// read-only-image + tmpfs live-system overlay, giving the worker a
// copy-on-write view whose writes are RAM-backed and discarded on Unmount.
func (kernelMounter) Mount(source, target string, opts MountOpts) error {
	if opts.SourceDir {
		// A classic bind mount cannot become read-only atomically: MS_BIND
		// creates it writable and a second remount changes the flag. Mount
		// propagation can deliver that first event to the worker without the
		// remount update. Make the hidden source a read-only mount first, so the
		// bind cloned onto target is born read-only. The source sits below target
		// and is hidden as soon as the final bind or overlay is established.
		if err := mount.BindDirectory(source, source); err != nil {
			return fmt.Errorf("protect directory lower: %w", err)
		}
	}
	if !opts.Writable {
		if opts.SourceDir {
			if err := mount.BindDirectory(source, target); err != nil {
				_ = unix.Unmount(source, 0)
				return err
			}
			return nil
		}
		return mountImage(source, target)
	}
	return mountOverlay(source, target, opts.SizeMiB, opts.UpperOnDisk, opts.SourceDir)
}

// Unmount unmounts target. For an overlay it also tears down the sibling scratch
// and squashfs lower set up by mountOverlay, in reverse order; read-only mounts
// have no siblings, so those steps are skipped. A scratch on disk was never a
// mount, so unmounting it fails harmlessly and the directory is removed as
// before. The auto-clear loop device is released once the squashfs mount goes
// away.
func (m kernelMounter) Unmount(target string) error {
	// Native sidecars may receive only two seconds after the worker exhausts
	// the pod's grace period. Share one retry budget across all mount layers.
	deadline := time.Now().Add(500 * time.Millisecond)
	mounted, err := m.IsMounted(target)
	if err != nil {
		return err
	}
	// A previous attempt may already have removed the artifact overlay. Never
	// unmount the underlying Kubernetes volume when retrying partial cleanup.
	if mounted {
		if err := mount.UnmountPath(target, deadline); err != nil {
			return err
		}
	}
	for _, path := range []string{overlayScratch(target), overlayLower(target)} {
		if err := mount.UnmountPath(path, deadline); err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove mount directory %s: %w", path, err)
		}
	}
	return nil
}

// IsMounted reports whether an artifact mount is stacked at target. Unlike a
// marker file, the kernel mount table survives a native-sidecar container
// restart and is authoritative for adoption.
//
// "Is a mount point" is not the question: the target may itself be a
// Kubernetes volume mount point (the runtime pod nests the code volume inside
// the workspace volume), so comparing mount IDs with the parent reports true
// for a bare volume that carries no artifact mount at all — and adopting that
// admits the workload against empty code. Instead the top mount entry at the
// path is inspected: an artifact mount is an overlay (writable), an image
// filesystem (squashfs, erofs), or a bind of an extracted tar tree, which is
// recognizable by its .lower root.
func (kernelMounter) IsMounted(target string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	return mount.HasArtifactMount(string(data), target), nil
}

func overlayLower(target string) string   { return mount.LowerDir(target) }
func overlayScratch(target string) string { return mount.ScratchDir(target) }

// mountImage opens image and mounts it read-only at target (mount.Image).
func mountImage(image, target string) error {
	img, err := os.Open(image)
	if err != nil {
		return fmt.Errorf("open image: %w", err)
	}
	defer img.Close()
	return mount.Image(img, target)
}

// mountOverlay loop-mounts the image read-only as the lower layer, mounts a
// tmpfs for the writable upper/work layers, then stacks an overlay at target.
// overlayfs requires upper and work on the same filesystem; tmpfs satisfies that
// and supports the trusted.overlay.* xattrs overlayfs needs. sizeMiB caps the
// tmpfs (0 = kernel default). On any failure it unwinds every mount and
// directory it created, leaving the workspace as it found it.
func mountOverlay(source, target string, sizeMiB int, upperOnDisk, sourceDir bool) (err error) {
	lower := overlayLower(target)
	scratch := overlayScratch(target)
	upper := mount.UpperDir(target)
	work := mount.WorkDir(target)

	// Unwind everything if we bail before the overlay is up; unmounting or
	// removing what was never created is harmless.
	defer func() {
		if err != nil {
			_ = unix.Unmount(scratch, 0)
			_ = unix.Unmount(lower, 0)
			_ = os.RemoveAll(scratch)
			_ = os.Remove(lower)
		}
	}()

	if sourceDir && filepath.Clean(source) != filepath.Clean(lower) {
		return fmt.Errorf("directory lower %s does not match mount lower %s", source, lower)
	}

	dirs := []string{scratch}
	if !sourceDir {
		dirs = append(dirs, lower)
	}
	for _, d := range dirs {
		if err = os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	if !sourceDir {
		if err = mountImage(source, lower); err != nil {
			return err
		}
	}

	// A synced upper stays on the workspace volume: the delta has to be an
	// ordinary directory the runner can archive, and it must survive every write
	// rather than the pod's memory. Otherwise tmpfs, which is RAM-backed and
	// counted against the pod's memory limit — a size cap turns an overrun into
	// ENOSPC instead of an OOM kill.
	if !upperOnDisk {
		if err = mount.Tmpfs(scratch, sizeMiB); err != nil {
			return err
		}
	}
	for _, d := range []string{upper, work} {
		if err = os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// overlayfs takes the merged root's ownership and mode from the upper layer,
	// so a root-owned 0755 upper makes a "writable" mount unwritable to a
	// workload running as anyone else — which every hardened workload does. This
	// mirrors what Kubernetes does to the emptyDir the mount lives in: world
	// writable, because the sidecar cannot know the image's uid. Chmod rather
	// than a mode on MkdirAll, which umask would clamp — and which would miss a
	// restored upper that already exists.
	if err = os.Chmod(upper, 0o777); err != nil {
		return fmt.Errorf("chmod %s: %w", upper, err)
	}

	return mount.Overlay(lower, upper, work, target)
}
