package artifact

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

// openWorkspace opens the workspace as an os.Root. Every artifact resolves its
// in and out paths through it, so a path can only ever name something inside
// the workspace: a symlink whose target leaves it fails instead of being
// followed, and so does an absolute target, which the root refuses even when
// it would land back inside. The workspace is shared with the workload, which
// runs before the post phase and can leave a link anywhere in it — and the
// sidecar reads and writes as root, with its own secrets in its environment.
func openWorkspace(basePath string) (*os.Root, error) {
	if err := os.MkdirAll(basePath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create workspace: %w", err)
	}
	root, err := os.OpenRoot(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open workspace: %w", err)
	}
	return root, nil
}

// fsName turns a workspace-relative OS path into the slash-separated, cleaned
// form io/fs names take, so a root's FS can be asked for it.
func fsName(name string) string {
	return path.Clean(filepath.ToSlash(name))
}

// openRegular opens a workspace file for reading without blocking on it and
// refusing anything but a regular file. The workspace is shared with the
// workload, which can leave a FIFO or device where an output is expected:
// opening one blocks the sidecar indefinitely (no deadline reaches a blocking
// open) or streams a boundless read. O_NONBLOCK returns straight away; the
// fstat on the open descriptor then judges the node the open actually landed
// on, not one a check could race.
func openRegular(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	return f, info, nil
}
