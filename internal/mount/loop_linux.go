//go:build linux

package mount

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// SetupLoop binds the already-open image to a free, read-only, auto-clearing
// loop device and returns its path (e.g. /dev/loop3) and the still-open device
// file. Taking an open file rather than a path lets a caller that resolved the
// image safely (see Root) hand over exactly the inode it checked. The caller
// must keep the device file open until the mount holds the device, then close
// it — with auto-clear the kernel detaches the device once its last reference
// drops, so an early close would pull the backing out from under the pending
// mount.
func SetupLoop(img *os.File) (string, *os.File, error) {
	control, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", nil, fmt.Errorf("open /dev/loop-control: %w", err)
	}
	defer control.Close()

	cfg := unix.LoopConfig{
		Fd:   uint32(img.Fd()),
		Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR},
	}
	copy(cfg.Info.File_name[:], img.Name()) // cosmetic: shows the backing file in losetup

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

// Image associates the open image with a loop device and mounts it
// read-only at target, picking the kernel filesystem type from the image's
// magic. The loop device is set to auto-clear, so unmounting releases it.
//
// The loop fd is held open across the mount: with auto-clear, the device
// detaches as soon as it has no holders, so it must stay open until the mount
// itself becomes the holder — otherwise the backing vanishes and mount fails
// with EIO.
func Image(img *os.File, target string) error {
	fstype, err := FsType(img)
	if err != nil {
		return err
	}

	loopPath, loop, err := SetupLoop(img)
	if err != nil {
		return err
	}
	defer loop.Close()

	if err := unix.Mount(loopPath, target, fstype, unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil {
		_ = unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0)
		return fmt.Errorf("mount %s on %s: %w", loopPath, target, err)
	}
	return nil
}
