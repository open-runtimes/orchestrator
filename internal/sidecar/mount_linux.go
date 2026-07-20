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
// loop device and the mount(2) syscall — no external binaries, so it works on a
// distroless image. It needs CAP_SYS_ADMIN, access to loop devices, and the
// matching filesystem kernel module (squashfs and/or erofs).
type kernelMounter struct{}

//nolint:iface // platform builds return different concrete Mounter types
func defaultMounter() Mounter { return kernelMounter{} }

// Mount mounts image at target. A read-only mount loop-mounts the image
// directly. A writable mount makes that read-only image the lower layer of an
// overlay whose upper/work layers live on a fresh tmpfs — the classic
// read-only-image + tmpfs live-system overlay, giving the worker a
// copy-on-write view whose writes are RAM-backed and discarded on Unmount.
func (kernelMounter) Mount(image, target string, opts MountOpts) error {
	if !opts.Writable {
		return mountImage(image, target)
	}
	return mountOverlay(image, target, opts.SizeMiB)
}

// Unmount unmounts target. For an overlay it also tears down the sibling tmpfs
// scratch and squashfs lower set up by mountOverlay, in reverse order; read-only
// mounts have no siblings, so those steps are skipped. The auto-clear loop
// device is released once the squashfs mount goes away.
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
		_ = os.Remove(lower)
	}
	return nil
}

// overlayLower/overlayScratch derive the sibling directories an overlay mount
// uses from its target, so Unmount can find them without extra bookkeeping.
func overlayLower(target string) string   { return target + ".lower" }
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
func mountOverlay(image, target string, sizeMiB int) (err error) {
	lower := overlayLower(target)
	scratch := overlayScratch(target)
	upper := filepath.Join(scratch, "upper")
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

	for _, d := range []string{lower, scratch} {
		if err = os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	if err = mountImage(image, lower); err != nil {
		return err
	}

	// tmpfs writes are RAM-backed and counted against the pod's memory limit; a
	// size cap turns an overrun into ENOSPC instead of an OOM kill.
	var tmpfsOpts string
	if sizeMiB > 0 {
		tmpfsOpts = fmt.Sprintf("size=%dm", sizeMiB)
	}
	if err = unix.Mount("tmpfs", scratch, "tmpfs", unix.MS_NODEV, tmpfsOpts); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %w", scratch, err)
	}
	for _, d := range []string{upper, work} {
		if err = os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
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
			// GET_FREE allocated the device but udev/devtmpfs hasn't created the
			// node yet; give it a moment and ask again.
			time.Sleep(10 * time.Millisecond)
			continue
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
