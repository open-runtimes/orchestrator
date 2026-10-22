//go:build linux

package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Root is a directory that every path is resolved strictly beneath. The node
// mount agent works inside a workspace the workload can write to, so a path it
// is asked to mount at may be swapped for a symlink to anywhere on the node
// between the request and the mount. Root never resolves a path by string:
// each lookup is an openat2 with RESOLVE_BENEATH (no escaping the root, ".."
// included) and RESOLVE_NO_SYMLINKS (no symlink anywhere in the path), and the
// kernel is then handed the opened inode as /proc/self/fd/N rather than the
// name, so what gets mounted on is exactly what was checked.
//
// Lookups do cross mount points: a mount stacked on a path inside the root is
// what the next lookup of that path returns, which is how the agent reaches
// the top of a mount it just made.
type Root struct {
	dir  *os.File
	path string
}

const resolveBeneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS

// OpenRoot opens path as a Root. The path itself is trusted: it is the
// workspace directory the agent created.
func OpenRoot(path string) (*Root, error) {
	dir, err := os.OpenFile(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open root %s: %w", path, err)
	}
	return &Root{dir: dir, path: filepath.Clean(path)}, nil
}

// Close releases the root directory.
func (r *Root) Close() error { return r.dir.Close() }

// Path is the root's own path, for matching against mountinfo. It must never be
// joined with a relative path and handed to a syscall; use the Open methods.
func (r *Root) Path() string { return r.path }

// ProcPath names an open file by descriptor. Passing it to mount(2) or
// umount(2) acts on the inode the descriptor holds, not on whatever the
// original path names now.
func ProcPath(f *os.File) string { return "/proc/self/fd/" + strconv.Itoa(int(f.Fd())) }

// Local validates rel as a path below the root: relative, no "..", and not the
// root itself, which the agent never mounts over.
func Local(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if !filepath.IsLocal(clean) || clean == "." {
		return "", fmt.Errorf("path %q is not inside the workspace", rel)
	}
	return clean, nil
}

func openBeneath(dir *os.File, rel string, flags int) (*os.File, error) {
	fd, err := unix.Openat2(int(dir.Fd()), rel, &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: resolveBeneath,
	})
	if err != nil {
		return nil, &os.PathError{Op: "openat2", Path: rel, Err: err}
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// OpenDir opens the directory at rel.
func (r *Root) OpenDir(rel string) (*os.File, error) {
	rel, err := Local(rel)
	if err != nil {
		return nil, err
	}
	return openBeneath(r.dir, rel, unix.O_RDONLY|unix.O_DIRECTORY)
}

// OpenFile opens the regular file at rel for reading.
func (r *Root) OpenFile(rel string) (*os.File, error) {
	rel, err := Local(rel)
	if err != nil {
		return nil, err
	}
	f, err := openBeneath(r.dir, rel, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return f, nil
}

// MkdirAll creates every missing directory along rel, one component at a time,
// each created and then opened relative to its parent's descriptor, and
// returns the last one open.
func (r *Root) MkdirAll(rel string, perm os.FileMode) (*os.File, error) {
	rel, err := Local(rel)
	if err != nil {
		return nil, err
	}
	return MkdirAllAt(r.dir, rel, perm)
}

// MkdirAllAt is MkdirAll relative to an already-open directory, which may be a
// mount the caller just stacked (and so cannot be reached through the Root's
// own path yet).
func MkdirAllAt(dir *os.File, rel string, perm os.FileMode) (*os.File, error) {
	cur, err := openBeneath(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	for name := range strings.SplitSeq(filepath.Clean(rel), "/") {
		next, err := openBeneath(cur, name, unix.O_RDONLY|unix.O_DIRECTORY)
		if errors.Is(err, unix.ENOENT) {
			if mkErr := unix.Mkdirat(int(cur.Fd()), name, uint32(perm)); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				cur.Close()
				return nil, &os.PathError{Op: "mkdirat", Path: name, Err: mkErr}
			}
			next, err = openBeneath(cur, name, unix.O_RDONLY|unix.O_DIRECTORY)
		}
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// Unmount unmounts the top mount at rel (see UnmountPath). It addresses the
// mount through its parent's descriptor — /proc/self/fd/<parent>/<name>, last
// component not followed — because a descriptor on the mount itself would be
// a reference to it, and the kernel would call the mount busy for as long as
// the caller held it. The parent is resolved beneath the root like any other
// path, so a swapped parent cannot redirect the unmount either.
func (r *Root) Unmount(rel string, deadline time.Time) error {
	rel, err := Local(rel)
	if err != nil {
		return err
	}
	parent := r.dir
	if d := filepath.Dir(rel); d != "." {
		p, err := openBeneath(r.dir, d, unix.O_RDONLY|unix.O_DIRECTORY)
		if err != nil {
			return err
		}
		defer p.Close()
		parent = p
	}
	return UnmountPath(ProcPath(parent)+"/"+filepath.Base(rel), deadline)
}

// RemoveAll removes rel and everything below it without following a symlink
// or leaving the filesystem rel lives on: anything still mounted underneath is
// left alone and reported, rather than emptied.
func (r *Root) RemoveAll(rel string) error {
	rel, err := Local(rel)
	if err != nil {
		return err
	}
	parent, err := openBeneath(r.dir, filepath.Dir(rel), unix.O_RDONLY|unix.O_DIRECTORY)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &st); err != nil {
		return err
	}
	return removeAt(parent, filepath.Base(rel), st.Dev)
}

func removeAt(parent *os.File, name string, dev uint64) error {
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unlinkAt(parent, name, 0)
	}
	if st.Dev != dev {
		return fmt.Errorf("%s is still mounted; not removing", name)
	}
	dir, err := openBeneath(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Read the whole listing before removing anything, so deletions cannot
	// shift what a later read returns.
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, child := range names {
		if err := removeAt(dir, child, dev); err != nil {
			return err
		}
	}
	return unlinkAt(parent, name, unix.AT_REMOVEDIR)
}

func unlinkAt(parent *os.File, name string, flags int) error {
	if err := unix.Unlinkat(int(parent.Fd()), name, flags); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}
