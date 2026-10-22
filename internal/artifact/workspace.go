package artifact

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
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
