//go:build linux

package mount

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// BindReadOnly attaches a read-only, nodev, nosuid bind of src onto dst, both
// given as open directories. The bind is cloned detached (open_tree), made
// read-only while detached (mount_setattr), and only then attached by
// descriptor (move_mount), so it is never visible writable — which the
// two-step bind-then-remount cannot promise once propagation is involved — and it lands on exactly the directory dst holds. Kernel 5.12+.
func BindReadOnly(src, dst *os.File) error {
	tree, err := unix.OpenTree(int(src.Fd()), "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH|unix.AT_RECURSIVE)
	if err != nil {
		return fmt.Errorf("clone %s: %w", src.Name(), err)
	}
	defer unix.Close(tree)
	attr := unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOSUID}
	if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &attr); err != nil {
		return fmt.Errorf("make %s read-only: %w", src.Name(), err)
	}
	if err := unix.MoveMount(tree, "", int(dst.Fd()), "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("attach %s on %s: %w", src.Name(), dst.Name(), err)
	}
	return nil
}

// Tmpfs mounts a fresh tmpfs at target, capped at sizeMiB (0 = kernel
// default). It is RAM-backed and counted against the memory limit of the cgroup
// that writes to it, so a cap turns an overrun into ENOSPC instead of an OOM
// kill.
func Tmpfs(target string, sizeMiB int) error {
	var opts string
	if sizeMiB > 0 {
		opts = fmt.Sprintf("size=%dm", sizeMiB)
	}
	if err := unix.Mount("tmpfs", target, "tmpfs", unix.MS_NODEV|unix.MS_NOSUID, opts); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %w", target, err)
	}
	return nil
}

// Overlay stacks an overlay at target. overlayfs requires upper and work on
// the same filesystem.
func Overlay(lower, upper, work, target string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := unix.Mount("overlay", target, "overlay", unix.MS_NOSUID, opts); err != nil {
		return fmt.Errorf("mount overlay on %s: %w", target, err)
	}
	return nil
}

// UnmountPath unmounts the top mount at path, retrying a busy mount until
// deadline and then detaching it. Busy references can belong to processes
// outside the caller (artifact mounts propagate), and a detached mount stays
// valid for them until they close it but no longer pins the mount point. A path
// that is not, or no longer, a mount point is not an error, so a retried
// cleanup is safe. The last component is never followed if it is a symlink.
func UnmountPath(path string, deadline time.Time) error {
	for {
		err := unix.Unmount(path, unix.UMOUNT_NOFOLLOW)
		if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("unmount %s: %w", path, err)
		}
		if time.Until(deadline) <= 0 {
			if err := unix.Unmount(path, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW); err != nil {
				return fmt.Errorf("detach busy mount %s: %w", path, err)
			}
			return nil
		}
		time.Sleep(min(50*time.Millisecond, time.Until(deadline)))
	}
}
