//go:build linux

package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

// Beneath performs artifact mounts inside a workspace directory: the sidecar's
// Mounter when it mounts in the pod, and the node mount agent's when it mounts
// from the host. Paths are relative to Dir and resolved through a Root on every
// step, because the workload shares the workspace and can rename or replace any
// entry at any time. After each mount the path is opened again, since a
// descriptor taken before a mount still points underneath it.
type Beneath struct {
	Dir string
}

// MaxMounts bounds the mounts one workspace may hold. Each image mount takes a
// loop device and every mount an entry in the node's mount table; a writable
// image mount uses three.
const MaxMounts = 64

// unmountBudget bounds how long a teardown waits on a busy mount before
// detaching it.
const unmountBudget = 500 * time.Millisecond

// Mount mounts source at target. A read-only mount loop-mounts an image (or
// binds a directory) at target directly; a writable mount makes it the lower
// layer of an overlay whose upper and work layers sit in ScratchDir(target).
func (b Beneath) Mount(source, target string, opts Opts) error {
	root, err := OpenRoot(b.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if source, err = Local(source); err != nil {
		return err
	}
	if target, err = Local(target); err != nil {
		return err
	}
	if n, err := countMountsUnder(root.Path()); err != nil {
		return err
	} else if n >= MaxMounts {
		return fmt.Errorf("workspace already holds %d mounts (limit %d)", n, MaxMounts)
	}
	t, err := root.MkdirAll(target, 0o755)
	if err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	defer t.Close()

	if opts.SourceDir {
		// Make the extracted directory lower read-only in place first: an
		// overlay over it, or a bind of it, then rests on a lower the workload
		// cannot change underneath.
		dir, err := root.OpenDir(source)
		if err != nil {
			return err
		}
		err = BindReadOnly(dir, dir)
		dir.Close()
		if err != nil {
			return fmt.Errorf("protect directory lower: %w", err)
		}
	}
	if opts.Writable {
		return mountOverlay(root, source, target, t, opts)
	}
	if opts.SourceDir {
		dir, err := root.OpenDir(source) // the read-only bind just stacked
		if err == nil {
			err = BindReadOnly(dir, t)
			dir.Close()
		}
		if err != nil {
			_ = unmountTop(root, source, time.Now().Add(unmountBudget))
		}
		return err
	}
	img, err := root.OpenFile(source)
	if err != nil {
		return fmt.Errorf("open image: %w", err)
	}
	defer img.Close()
	return Image(img, ProcPath(t))
}

func mountOverlay(root *Root, source, target string, t *os.File, opts Opts) (err error) {
	lower, scratch := LowerDir(target), ScratchDir(target)
	defer func() {
		if err != nil {
			deadline := time.Now().Add(unmountBudget)
			_ = unmountTop(root, scratch, deadline)
			_ = unmountTop(root, lower, deadline)
			_ = root.RemoveAll(scratch)
			if !opts.SourceDir {
				_ = root.RemoveAll(lower)
			}
		}
	}()

	if opts.SourceDir && filepath.Clean(source) != lower {
		return fmt.Errorf("directory lower %s does not match mount lower %s", source, lower)
	}
	if !opts.SourceDir {
		l, err := root.MkdirAll(lower, 0o755)
		if err != nil {
			return err
		}
		img, err := root.OpenFile(source)
		if err != nil {
			l.Close()
			return fmt.Errorf("open image: %w", err)
		}
		err = Image(img, ProcPath(l))
		img.Close()
		l.Close()
		if err != nil {
			return err
		}
	}

	s, err := root.MkdirAll(scratch, 0o755)
	if err != nil {
		return err
	}
	// A synced upper stays on the workspace: the delta has to be an ordinary
	// directory the runner can archive. Otherwise tmpfs.
	if !opts.UpperOnDisk {
		err = Tmpfs(ProcPath(s), opts.SizeMiB)
	}
	s.Close()
	if err != nil {
		return err
	}

	// Reopen what the overlay is built from: the lower and scratch descriptors
	// taken above point underneath the mounts just stacked on them.
	l, err := root.OpenDir(lower)
	if err != nil {
		return err
	}
	defer l.Close()
	s, err = root.OpenDir(scratch)
	if err != nil {
		return err
	}
	defer s.Close()
	upper, err := MkdirAllAt(s, "upper", 0o755)
	if err != nil {
		return err
	}
	defer upper.Close()
	work, err := MkdirAllAt(s, "work", 0o755)
	if err != nil {
		return err
	}
	defer work.Close()

	// overlayfs takes the merged root's ownership and mode from the upper, so a
	// root-owned 0755 upper makes a "writable" mount unwritable to a workload
	// running as anyone else. World-writable, like the volume it lives in.
	if err := unix.Fchmod(int(upper.Fd()), 0o777); err != nil {
		return fmt.Errorf("chmod upper: %w", err)
	}
	return Overlay(ProcPath(l), ProcPath(upper), ProcPath(work), ProcPath(t))
}

// Unmount tears down a mount at target and the scratch and lower beside it,
// innermost first, then removes their directories. Partial state from an
// earlier failure is fine: missing pieces are skipped. The target directory
// itself is left in place.
func (b Beneath) Unmount(target string) error {
	root, err := OpenRoot(b.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if target, err = Local(target); err != nil {
		return err
	}
	deadline := time.Now().Add(unmountBudget)
	mounted, err := b.IsMounted(target)
	if err != nil {
		return err
	}
	if mounted {
		if err := unmountTop(root, target, deadline); err != nil {
			return err
		}
	}
	for _, p := range []string{ScratchDir(target), LowerDir(target)} {
		if err := unmountTop(root, p, deadline); err != nil {
			return err
		}
		if err := root.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

// IsMounted reports whether an artifact mount is stacked at target. Unlike a
// marker file, the kernel mount table survives a native-sidecar container
// restart and is authoritative for adoption. Only names are compared, so
// nothing is resolved.
func (b Beneath) IsMounted(target string) (bool, error) {
	target, err := Local(target)
	if err != nil {
		return false, err
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	return HasArtifactMount(string(info), filepath.Join(filepath.Clean(b.Dir), target)), nil
}

// unmountTop removes the top mount at rel, if rel is a mount point. The mount
// point is checked by name in mountinfo and then unmounted through the Root, so
// a swapped path can neither redirect the unmount nor reach outside it.
func unmountTop(root *Root, rel string, deadline time.Time) error {
	isMount, err := IsMountPoint(filepath.Join(root.Path(), rel))
	if err != nil || !isMount {
		return err
	}
	err = root.Unmount(rel, deadline)
	if errors.Is(err, unix.ENOENT) {
		return nil // gone between the mountinfo read and the unmount
	}
	return err
}

// IsMountPoint reports whether path is a mount point, by name in mountinfo.
func IsMountPoint(path string) (bool, error) {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	path = filepath.Clean(path)
	return slices.Contains(MountsUnder(string(info), path), path), nil
}

func countMountsUnder(dir string) (int, error) {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, fmt.Errorf("read mountinfo: %w", err)
	}
	n := 0
	for _, m := range MountsUnder(string(info), dir) {
		if m != dir {
			n++
		}
	}
	return n, nil
}
