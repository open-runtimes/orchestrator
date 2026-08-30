//go:build linux

package sidecar

import (
	"errors"
	"fmt"
	"io"
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
		if err := mountDirectory(source, source); err != nil {
			return fmt.Errorf("protect directory lower: %w", err)
		}
	}
	if !opts.Writable {
		if opts.SourceDir {
			if err := mountDirectory(source, target); err != nil {
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
func (kernelMounter) Unmount(target string) error {
	if err := unix.Unmount(target, 0); err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	scratch := overlayScratch(target)
	if _, err := os.Stat(scratch); err == nil {
		_ = unix.Unmount(scratch, 0)
		_ = os.RemoveAll(scratch)
	}
	lower := overlayLower(target)
	if _, err := os.Stat(lower); err == nil {
		_ = unix.Unmount(lower, 0)
		_ = os.RemoveAll(lower)
	}
	return nil
}

// overlayLower lives inside the directory that is about to become the mount
// point. Once the bind or overlay is established at target, the implementation
// lower is hidden from the worker; unmounting target reveals it for cleanup.
// overlayScratch remains a sibling because a writable overlay must keep its
// upper and work directories accessible to the sidecar for sync.
func overlayLower(target string) string   { return filepath.Join(target, ".lower") }
func overlayScratch(target string) string { return target + ".scratch" }

// mountImage associates image with a free loop device and mounts it read-only
// at target, picking the kernel filesystem type from the image's magic. The
// loop device is set to auto-clear, so unmounting releases it.
//
// The loop fd is held open across the mount: with auto-clear, the device
// detaches as soon as it has no holders, so it must stay open until the mount
// itself becomes the holder — otherwise the backing vanishes and mount fails
// with EIO.
func mountImage(image, target string) error {
	fstype, err := imageFsType(image)
	if err != nil {
		return err
	}

	loopPath, loop, err := setupLoop(image)
	if err != nil {
		return err
	}
	defer loop.Close()

	if err := unix.Mount(loopPath, target, fstype, unix.MS_RDONLY|unix.MS_NODEV, ""); err != nil {
		_ = unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0)
		return fmt.Errorf("mount %s on %s: %w", loopPath, target, err)
	}
	return nil
}

// mountDirectory exposes an extracted tar tree with the same read-only
// contract as a mounted filesystem image. The second mount call remounts the
// bind read-only; a plain bind inherits the source's writable flags.
func mountDirectory(source, target string) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount %s on %s: %w", source, target, err)
	}
	if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil {
		_ = unix.Unmount(target, 0)
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}

// imageFsType sniffs the on-disk magic to choose the kernel filesystem type for
// image: squashfs ("hsqs" at offset 0) or erofs (0xE0F5E1E2, little-endian, at
// offset 1024). Both are read-only image formats loop-mounted identically
// apart from the type string.
func imageFsType(image string) (string, error) {
	f, err := os.Open(image)
	if err != nil {
		return "", fmt.Errorf("open image: %w", err)
	}
	defer f.Close()

	// erofs's magic sits at offset 1024, so read past it.
	buf := make([]byte, 1028)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]

	switch {
	case len(buf) >= 4 && string(buf[:4]) == "hsqs":
		return "squashfs", nil
	case len(buf) >= 1028 && buf[1024] == 0xe2 && buf[1025] == 0xe1 && buf[1026] == 0xf5 && buf[1027] == 0xe0:
		return "erofs", nil
	default:
		return "", fmt.Errorf("unrecognized filesystem image %s", image)
	}
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
	upper := UpperDir(target)
	work := filepath.Join(scratch, "work")

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
		var tmpfsOpts string
		if sizeMiB > 0 {
			tmpfsOpts = fmt.Sprintf("size=%dm", sizeMiB)
		}
		if err = unix.Mount("tmpfs", scratch, "tmpfs", unix.MS_NODEV, tmpfsOpts); err != nil {
			return fmt.Errorf("mount tmpfs on %s: %w", scratch, err)
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

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err = unix.Mount("overlay", target, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay on %s: %w", target, err)
	}
	return nil
}

// setupLoop binds image to a free, read-only, auto-clearing loop device and
// returns its path (e.g. /dev/loop3) and the still-open device file. The caller
// must keep the file open until the mount holds the device, then close it —
// with auto-clear the kernel detaches the device once its last reference drops,
// so an early close would pull the backing out from under the pending mount.
func setupLoop(image string) (string, *os.File, error) {
	img, err := os.OpenFile(image, os.O_RDONLY, 0)
	if err != nil {
		return "", nil, fmt.Errorf("open image: %w", err)
	}
	defer img.Close()

	control, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", nil, fmt.Errorf("open /dev/loop-control: %w", err)
	}
	defer control.Close()

	cfg := unix.LoopConfig{
		Fd:   uint32(img.Fd()),
		Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR},
	}
	copy(cfg.Info.File_name[:], image) // cosmetic: shows the backing file in losetup

	// There is no atomic "allocate + attach": LOOP_CTL_GET_FREE only reports a
	// device that was free, and another mounter on the node can claim it before
	// we configure it. LOOP_CONFIGURE then fails with EBUSY, so we grab a fresh
	// device and retry (bounded) — the same contention-at-attach contract
	// losetup relies on. LOOP_CONFIGURE (kernel 5.8+, always present under the
	// erofs/squashfs floor) sets fd, flags, and geometry in one ioctl, leaving
	// no half-configured window for a concurrent scanner to observe.
	for range 100 {
		num, err := unix.IoctlRetInt(int(control.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return "", nil, fmt.Errorf("LOOP_CTL_GET_FREE: %w", err)
		}
		loopPath := fmt.Sprintf("/dev/loop%d", num)

		loop, err := os.OpenFile(loopPath, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			// GET_FREE allocated the device, but a container's /dev is a static
			// snapshot taken when it started: a node the kernel creates later
			// never appears in it, so waiting cannot help — GET_FREE would keep
			// naming the same unopenable device until the retries run out.
			// Create the node ourselves, the way losetup recovers a lost device
			// node (mknod is covered by the same privilege the mount needs).
			// Losing the race to another creator (EEXIST) is fine — the node is
			// there either way. Where mknod is refused, fall back to a brief
			// wait for udev/devtmpfs, the only remaining way the node can appear.
			if mkErr := unix.Mknod(loopPath, unix.S_IFBLK|0o660, int(unix.Mkdev(7, uint32(num)))); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			loop, err = os.OpenFile(loopPath, os.O_RDWR, 0)
		}
		if err != nil {
			return "", nil, fmt.Errorf("open %s: %w", loopPath, err)
		}

		err = unix.IoctlLoopConfigure(int(loop.Fd()), &cfg)
		if err == nil {
			return loopPath, loop, nil
		}
		loop.Close()
		if errors.Is(err, unix.EBUSY) {
			continue // raced with another mounter; grab a different device
		}
		return "", nil, fmt.Errorf("LOOP_CONFIGURE: %w", err)
	}
	return "", nil, errors.New("no free loop device after retries")
}
