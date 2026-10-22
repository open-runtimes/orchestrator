//go:build linux

package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Beneath performs artifact mounts inside a Root from outside the pod: the
// node mount agent's half of the job the privileged in-pod sidecar does today.
// The layout is identical (LowerDir, ScratchDir, UpperDir), so a runner that
// syncs an upper or adopts existing mounts cannot tell which one did the work.
//
// Paths are relative to the Root and resolved through it on every step,
// because the workload shares the workspace and can rename or replace any
// entry at any time. After each mount the path is opened again, since a
// descriptor taken before a mount still points underneath it.
type Beneath struct {
	Root *Root
}

// unmountBudget bounds how long a teardown waits on a busy mount before
// detaching it.
const unmountBudget = 500 * time.Millisecond

// Mount mounts source at target. A read-only mount loop-mounts an image (or
// binds a directory) at target directly; a writable mount makes it the lower
// layer of an overlay whose upper and work layers sit in ScratchDir(target).
func (b Beneath) Mount(source, target string, opts Opts) (err error) {
	if source, err = Local(source); err != nil {
		return err
	}
	if target, err = Local(target); err != nil {
		return err
	}
	t, err := b.Root.MkdirAll(target, 0o755)
	if err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	defer t.Close()

	if opts.SourceDir {
		// Make the extracted directory lower read-only in place first, as the
		// in-pod mounter does: an overlay over it, or a bind of it, then rests
		// on a lower the workload cannot change underneath.
		if err := b.onDir(source, func(dir *os.File) error { return BindReadOnly(dir, dir) }); err != nil {
			return fmt.Errorf("protect directory lower: %w", err)
		}
	}
	if opts.Writable {
		return b.mountOverlay(source, target, t, opts)
	}
	if opts.SourceDir {
		err := b.onDir(source, func(dir *os.File) error { return BindReadOnly(dir, t) })
		if err != nil {
			_ = b.unmountTop(source, time.Now().Add(unmountBudget))
		}
		return err
	}
	img, err := b.Root.OpenFile(source)
	if err != nil {
		return fmt.Errorf("open image: %w", err)
	}
	defer img.Close()
	return Image(img, ProcPath(t))
}

func (b Beneath) mountOverlay(source, target string, t *os.File, opts Opts) (err error) {
	lower, scratch := LowerDir(target), ScratchDir(target)
	defer func() {
		if err != nil {
			deadline := time.Now().Add(unmountBudget)
			_ = b.unmountTop(scratch, deadline)
			_ = b.unmountTop(lower, deadline)
			_ = b.Root.RemoveAll(scratch)
			if !opts.SourceDir {
				_ = b.Root.RemoveAll(lower)
			}
		}
	}()

	if opts.SourceDir && filepath.Clean(source) != lower {
		return fmt.Errorf("directory lower %s does not match mount lower %s", source, lower)
	}
	if !opts.SourceDir {
		l, err := b.Root.MkdirAll(lower, 0o755)
		if err != nil {
			return err
		}
		img, err := b.Root.OpenFile(source)
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

	s, err := b.Root.MkdirAll(scratch, 0o755)
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

	// Reopen everything the overlay is built from: the lower and scratch
	// descriptors taken above point underneath the mounts just stacked on them.
	l, err := b.Root.OpenDir(lower)
	if err != nil {
		return err
	}
	defer l.Close()
	s, err = b.Root.OpenDir(scratch)
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
// itself is left in place, as the in-pod mounter leaves it.
func (b Beneath) Unmount(target string) error {
	target, err := Local(target)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(unmountBudget)
	mounted, err := b.IsMounted(target)
	if err != nil {
		return err
	}
	if mounted {
		if err := b.unmountTop(target, deadline); err != nil {
			return err
		}
	}
	for _, p := range []string{ScratchDir(target), LowerDir(target)} {
		if err := b.unmountTop(p, deadline); err != nil {
			return err
		}
		if err := b.Root.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

// IsMounted reports whether an artifact mount is stacked at target, by the
// same mountinfo test the in-pod mounter uses. Only names are compared here,
// so nothing is resolved.
func (b Beneath) IsMounted(target string) (bool, error) {
	target, err := Local(target)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	return HasArtifactMount(string(data), filepath.Join(b.Root.Path(), target)), nil
}

// unmountTop removes the top mount at rel, if rel exists and is a mount point.
// The mount point is checked by name in mountinfo and then unmounted through a
// descriptor, so a swapped path can neither redirect the unmount nor make the
// agent unmount something that is not inside the root.
func (b Beneath) unmountTop(rel string, deadline time.Time) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}
	full := filepath.Join(b.Root.Path(), rel)
	found := false
	for _, m := range MountsUnder(string(data), full) {
		if m == full {
			found = true
		}
	}
	if !found {
		return nil
	}
	err = b.Root.Unmount(rel, deadline)
	if errors.Is(err, unix.ENOENT) {
		return nil // gone between the mountinfo read and the open
	}
	return err
}

func (b Beneath) onDir(rel string, fn func(*os.File) error) error {
	dir, err := b.Root.OpenDir(rel)
	if err != nil {
		return err
	}
	defer dir.Close()
	return fn(dir)
}
