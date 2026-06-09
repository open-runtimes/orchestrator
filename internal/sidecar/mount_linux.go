//go:build linux

package sidecar

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// kernelMounter mounts squashfs images via a loop device and the mount(2)
// syscall — no external binaries, so it works on a distroless image. It needs
// CAP_SYS_ADMIN, access to loop devices, and the squashfs kernel module.
type kernelMounter struct{}

//nolint:iface // platform builds return different concrete Mounter types
func defaultMounter() Mounter { return kernelMounter{} }

// Mount associates image with a free loop device and mounts it read-only at
// target. The loop device is set to auto-clear, so Unmount releases it.
//
// The loop fd is held open across the mount: with auto-clear, the device
// detaches as soon as it has no holders, so it must stay open until the mount
// itself becomes the holder — otherwise the backing vanishes and mount fails
// with EIO.
func (kernelMounter) Mount(image, target string) error {
	loopPath, loop, err := setupLoop(image)
	if err != nil {
		return err
	}
	defer loop.Close()

	if err := unix.Mount(loopPath, target, "squashfs", unix.MS_RDONLY|unix.MS_NODEV, ""); err != nil {
		_ = unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0)
		return fmt.Errorf("mount %s on %s: %w", loopPath, target, err)
	}
	return nil
}

// Unmount unmounts target; the auto-clear loop device is released automatically.
func (kernelMounter) Unmount(target string) error {
	if err := unix.Unmount(target, 0); err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}

// setupLoop binds image to a free, read-only, auto-clearing loop device and
// returns its path (e.g. /dev/loop3) and the still-open device file. The caller
// must keep the file open until the mount holds the device, then close it.
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

	// GET_FREE then SET_FD is a TOCTOU window: another mounter on the node can
	// claim the same device in between, so SET_FD returns EBUSY. Retry with a
	// fresh device (bounded) — this is what losetup does.
	for range 100 {
		num, err := unix.IoctlRetInt(int(control.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return "", nil, fmt.Errorf("LOOP_CTL_GET_FREE: %w", err)
		}
		loopPath := fmt.Sprintf("/dev/loop%d", num)

		loop, err := os.OpenFile(loopPath, os.O_RDWR, 0)
		if err != nil {
			return "", nil, fmt.Errorf("open %s: %w", loopPath, err)
		}

		if err := unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_SET_FD, int(img.Fd())); err != nil {
			loop.Close()
			if errors.Is(err, unix.EBUSY) {
				continue // raced with another mounter; grab a different device
			}
			return "", nil, fmt.Errorf("LOOP_SET_FD: %w", err)
		}

		info := unix.LoopInfo64{Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR}
		if err := unix.IoctlLoopSetStatus64(int(loop.Fd()), &info); err != nil {
			_ = unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0)
			loop.Close()
			return "", nil, fmt.Errorf("LOOP_SET_STATUS64: %w", err)
		}
		return loopPath, loop, nil
	}
	return "", nil, errors.New("no free loop device after retries")
}
