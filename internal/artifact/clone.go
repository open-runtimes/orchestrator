package artifact

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

const defaultCloneTimeoutSeconds = 300 // 5 minutes

// Clone materializes the tree at a git ref — not a repository: the clone is
// shallow, single-ref, tagless, and the .git directory is removed once the
// tree is checked out. For providers that hand out archive URLs, download +
// unarchive is the cheaper pipeline; clone exists for the providers that only
// serve content over the git protocol.
type Clone struct {
	ID  string `json:"id"`
	In  string `json:"in"`  // Repository URL (http/https, no credentials — see Headers)
	Out string `json:"out"` // Directory to materialize the tree into
	// Ref is a branch name, tag name, or full commit hash; empty means the
	// remote's default branch. A 40-hex ref is treated as a commit hash, which
	// reaches any commit the server is willing to serve (git forges generally
	// allow reachable commits), not just branch and tag tips.
	Ref            string            `json:"ref,omitempty"`
	Subdir         string            `json:"subdir,omitempty"` // Keep only this subdirectory of the tree
	Depends        string            `json:"depends,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"` // Timeout in seconds for the whole clone (default 300)
	Headers        map[string]string `json:"headers,omitempty"`        // Sent with every git request, e.g. Authorization

	// noShallow drops the depth limit. Only tests set it: the in-process
	// transport they serve fixtures over rejects shallow fetches, which every
	// real git server accepts.
	noShallow bool
}

// depth returns the fetch depth: shallow unless the test seam says otherwise.
func (a *Clone) depth() int {
	if a.noShallow {
		return 0
	}
	return 1
}

func (a *Clone) ArtifactID() string   { return a.ID }
func (a *Clone) ArtifactType() string { return "clone" }
func (a *Clone) DependsOn() string    { return a.Depends }

// cloneAuth sends caller-supplied headers with every git request. Credentials
// ride a header instead of URL userinfo so they can never surface in the
// errors and logs that echo the URL; String() is what go-git prints.
type cloneAuth map[string]string

func (cloneAuth) Name() string   { return "headers" }
func (cloneAuth) String() string { return "headers" }
func (h cloneAuth) SetAuth(r *http.Request) {
	for k, v := range h {
		r.Header.Set(k, v)
	}
}

// Apply materializes the tree at the requested ref into Out.
//
// The clone lands in a scratch directory under the workspace rather than in
// the destination itself. Nothing here may erase what the workspace already
// holds at Out — an earlier artifact's output, or a ref that only resolves on
// the second attempt would take it with it — so the tree is moved into place
// at the end, merging the way unarchive extracts into an existing directory.
func (a *Clone) Apply(ctx context.Context, basePath string) *Result {
	destPath := filepath.Join(basePath, a.Out)

	if err := os.MkdirAll(basePath, 0o755); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create directory: %w", err)}
	}

	// The scratch directory hangs off the workspace root rather than the
	// destination's parent: that parent is the workspace itself when Out is
	// ".", and a scratch directory above the workspace could land on another
	// filesystem, where moving the tree into place would stop being a rename.
	workPath, err := os.MkdirTemp(basePath, ".clone-*")
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create the clone directory: %w", err)}
	}
	defer func() {
		if err := os.RemoveAll(workPath); err != nil {
			slog.Warn("Failed to remove the clone directory", "path", workPath, "error", err)
		}
	}()

	timeoutSecs := a.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = defaultCloneTimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	var auth transport.AuthMethod
	if len(a.Headers) > 0 {
		auth = cloneAuth(a.Headers)
	}

	if isCommitHash(a.Ref) {
		err = a.cloneCommit(ctx, workPath, auth)
	} else {
		err = a.cloneRef(ctx, workPath, auth)
	}
	if err != nil {
		return &Result{Status: "failed", Error: err}
	}

	if err := os.RemoveAll(filepath.Join(workPath, git.GitDirName)); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to remove the .git directory: %w", err)}
	}

	root, err := sourceRoot(workPath, a.Subdir)
	if err != nil {
		return &Result{Status: "failed", Error: err}
	}

	if err := moveTree(root, destPath); err != nil {
		return &Result{Status: "failed", Error: err}
	}

	slog.Debug("Cloned repository", "url", a.In, "ref", a.Ref, "path", destPath)
	return &Result{Status: "success"}
}

// cloneRef clones a branch, a tag, or the remote's default branch. A name is
// tried as a branch first and as a tag second — the order every git tool
// resolves the ambiguity in.
func (a *Clone) cloneRef(ctx context.Context, workPath string, auth transport.AuthMethod) error {
	options := &git.CloneOptions{
		URL:          a.In,
		Auth:         auth,
		Depth:        a.depth(),
		SingleBranch: true,
		Tags:         git.NoTags,
	}

	if a.Ref == "" {
		_, err := git.PlainCloneContext(ctx, workPath, false, options)
		if err != nil {
			return fmt.Errorf("failed to clone the default branch: %w", err)
		}
		return nil
	}

	options.ReferenceName = plumbing.NewBranchReferenceName(a.Ref)
	_, branchErr := git.PlainCloneContext(ctx, workPath, false, options)
	if branchErr == nil {
		return nil
	}

	// A failed attempt leaves a partial .git behind, which the retry would
	// refuse as an existing repository. Emptying the scratch directory is safe
	// where emptying the destination would not be: nothing but this attempt
	// has ever written here.
	if err := resetDir(workPath); err != nil {
		return err
	}

	options.ReferenceName = plumbing.NewTagReferenceName(a.Ref)
	if _, tagErr := git.PlainCloneContext(ctx, workPath, false, options); tagErr != nil {
		return fmt.Errorf("failed to clone ref %q as a branch (%w) or a tag (%w)", a.Ref, branchErr, tagErr)
	}
	return nil
}

// cloneCommit fetches a single commit by hash and checks out its tree. Served
// only when the remote allows non-tip wants, which the git forges do for
// reachable commits.
func (a *Clone) cloneCommit(ctx context.Context, workPath string, auth transport.AuthMethod) error {
	repo, err := git.PlainInit(workPath, false)
	if err != nil {
		return fmt.Errorf("failed to init repository: %w", err)
	}

	remote, err := repo.CreateRemote(&config.RemoteConfig{
		Name: git.DefaultRemoteName,
		URLs: []string{a.In},
	})
	if err != nil {
		return fmt.Errorf("failed to add remote: %w", err)
	}

	err = remote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(a.Ref + ":refs/heads/detached")},
		Depth:    a.depth(),
		Auth:     auth,
		Tags:     git.NoTags,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch commit %s: %w", a.Ref, err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to open worktree: %w", err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(a.Ref)}); err != nil {
		return fmt.Errorf("failed to check out commit %s: %w", a.Ref, err)
	}
	return nil
}

// sourceRoot returns the directory in the clone whose contents belong in the
// destination: the clone root, or the subdir named within it. A subdir that
// matches nothing fails rather than succeeding with the wrong tree — the same
// contract unarchive's subdir keeps.
func sourceRoot(clonePath, subdir string) (string, error) {
	subdir = cleanSubdir(subdir)
	if subdir == "" {
		return clonePath, nil
	}

	subdirPath := filepath.Join(clonePath, filepath.FromSlash(subdir))
	info, err := os.Stat(subdirPath)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("subdir %q does not name a directory in the cloned tree", subdir)
	}
	return subdirPath, nil
}

// moveTree moves the contents of src into dst, which it creates if missing.
// Directory meets directory merges; anything else replaces what it lands on,
// leaving the rest of dst alone. src is a scratch directory on the same
// filesystem, so every entry moves with a rename rather than a copy.
func moveTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", dst, err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("failed to read the cloned tree: %w", err)
	}

	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())

		if existing, err := os.Lstat(to); err == nil {
			if entry.IsDir() && existing.IsDir() {
				if err := moveTree(from, to); err != nil {
					return err
				}
				continue
			}
			if err := os.RemoveAll(to); err != nil {
				return fmt.Errorf("failed to replace %s: %w", to, err)
			}
		}

		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("failed to move %s into place: %w", to, err)
		}
	}

	return nil
}

// resetDir empties a directory without removing the directory itself.
func resetDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to reset the clone directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to reset the clone directory: %w", err)
	}
	return nil
}

// isCommitHash reports whether ref is a full 40-hex commit hash. A branch or
// tag that happens to carry such a name loses to the commit reading, the same
// ambiguity rule git itself applies.
func isCommitHash(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
