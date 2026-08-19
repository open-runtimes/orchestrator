package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

// The in-process server transport does not advertise SHA1-in-want the way the
// real git forges do, so the fixture wraps it to add the capability — the
// go-git client refuses to send a commit-hash want without seeing it.
type shaCapableTransport struct{ transport.Transport }

func (t shaCapableTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	session, err := t.Transport.NewUploadPackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	return shaCapableSession{session}, nil
}

type shaCapableSession struct{ transport.UploadPackSession }

func (s shaCapableSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(context.Background())
}

func (s shaCapableSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	refs, err := s.UploadPackSession.AdvertisedReferencesContext(ctx)
	if err == nil {
		if capErr := refs.Capabilities.Set(capability.AllowReachableSHA1InWant); capErr != nil {
			return nil, capErr
		}
	}
	return refs, err
}

// serveFixture builds a repository and serves it over go-git's in-process
// transport as fixtureURL. The repository has a default branch with a root
// file and an app/web subdirectory, a "feature" branch adding feature.txt,
// and a "v1" tag on the initial commit. The in-process transport rejects
// shallow fetches, so the Apply tests set noShallow on their artifacts.
const fixtureURL = "http://gitserver/fixture.git"

func serveFixture(t *testing.T) (initial, feature plumbing.Hash) {
	t.Helper()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit() error = %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}

	signature := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}

	writeFixtureFile(t, dir, "main.txt", "main content")
	writeFixtureFile(t, dir, "app/web/index.txt", "web content")
	if _, err := worktree.Add("."); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	initial, err = worktree.Commit("initial", &git.CommitOptions{Author: signature})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if _, err := repo.CreateTag("v1", initial, nil); err != nil {
		t.Fatalf("CreateTag() error = %v", err)
	}

	defaultBranch := headBranch(t, repo)
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature"), Create: true}); err != nil {
		t.Fatalf("Checkout(feature) error = %v", err)
	}
	writeFixtureFile(t, dir, "feature.txt", "feature content")
	if _, err := worktree.Add("."); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	feature, err = worktree.Commit("feature", &git.CommitOptions{Author: signature})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: defaultBranch}); err != nil {
		t.Fatalf("Checkout(default) error = %v", err)
	}

	client.InstallProtocol("http", shaCapableTransport{server.NewClient(server.MapLoader{fixtureURL: repo.Storer})})
	t.Cleanup(func() {
		client.InstallProtocol("http", githttp.DefaultClient)
	})

	return initial, feature
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func headBranch(t *testing.T, repo *git.Repository) plumbing.ReferenceName {
	t.Helper()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	return head.Name()
}

func assertFile(t *testing.T, base, name, content string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("expected file %s: %v", name, err)
	}
	if string(got) != content {
		t.Errorf("file %s = %q, want %q", name, got, content)
	}
}

func assertAbsent(t *testing.T, base, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(name))); !os.IsNotExist(err) {
		t.Errorf("expected %s to be absent, stat err = %v", name, err)
	}
}

func TestClone_Interface(t *testing.T) {
	a := &Clone{ID: "cl1", In: "https://example.com/repo.git", Out: "src", Depends: "other"}
	if a.ArtifactID() != "cl1" {
		t.Errorf("ArtifactID() = %v, want cl1", a.ArtifactID())
	}
	if a.ArtifactType() != "clone" {
		t.Errorf("ArtifactType() = %v, want clone", a.ArtifactType())
	}
	if a.DependsOn() != "other" {
		t.Errorf("DependsOn() = %v, want other", a.DependsOn())
	}
}

func TestClone_Apply_DefaultBranch(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	dest := filepath.Join(tmpDir, "src")
	assertFile(t, dest, "main.txt", "main content")
	assertFile(t, dest, "app/web/index.txt", "web content")
	assertAbsent(t, dest, "feature.txt")
	assertAbsent(t, dest, ".git")
}

func TestClone_Apply_Branch(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Ref: "feature"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	dest := filepath.Join(tmpDir, "src")
	assertFile(t, dest, "feature.txt", "feature content")
	assertAbsent(t, dest, ".git")
}

func TestClone_Apply_Tag(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Ref: "v1"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	dest := filepath.Join(tmpDir, "src")
	assertFile(t, dest, "main.txt", "main content")
	assertAbsent(t, dest, "feature.txt")
}

func TestClone_Apply_Commit(t *testing.T) {
	_, feature := serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Ref: feature.String()}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	dest := filepath.Join(tmpDir, "src")
	assertFile(t, dest, "feature.txt", "feature content")
	assertAbsent(t, dest, ".git")
}

func TestClone_Apply_Subdir(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Subdir: "./app/web/"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	dest := filepath.Join(tmpDir, "src")
	assertFile(t, dest, "index.txt", "web content")
	assertAbsent(t, dest, "main.txt")
	assertAbsent(t, dest, "app")
}

func TestClone_Apply_KeepsExistingDestination(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	// A tag ref only resolves on the second attempt, so this is the case that
	// used to empty the destination before retrying.
	dest := filepath.Join(tmpDir, "src")
	writeFixtureFile(t, dest, "keep.txt", "earlier artifact")
	writeFixtureFile(t, dest, "app/web/keep.txt", "earlier artifact, nested")
	writeFixtureFile(t, dest, "main.txt", "stale")

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Ref: "v1"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	// Untouched by the clone, merged beside it, and overwritten by it.
	assertFile(t, dest, "keep.txt", "earlier artifact")
	assertFile(t, dest, "app/web/keep.txt", "earlier artifact, nested")
	assertFile(t, dest, "app/web/index.txt", "web content")
	assertFile(t, dest, "main.txt", "main content")
}

func TestClone_Apply_RemovesScratchDirectory(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src"}
	if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "src" {
			t.Errorf("expected only the destination to remain, found %q", entry.Name())
		}
	}
}

func TestClone_Apply_FailedCloneLeavesDestinationAlone(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	dest := filepath.Join(tmpDir, "src")
	writeFixtureFile(t, dest, "keep.txt", "earlier artifact")

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Ref: "no-such-ref"}
	if result := a.Apply(t.Context(), tmpDir); result.Error == nil {
		t.Fatal("Apply() expected an error for an unknown ref")
	}

	assertFile(t, dest, "keep.txt", "earlier artifact")
}

func TestClone_Apply_WorkspaceRootDestination(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	// "." names the workspace itself: the scratch directory must stay inside
	// it rather than landing beside it.
	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "."}
	if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	assertFile(t, tmpDir, "main.txt", "main content")
	assertAbsent(t, tmpDir, ".git")

	entries, err := os.ReadDir(filepath.Dir(tmpDir))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".clone-") {
			t.Errorf("scratch directory %q escaped the workspace", entry.Name())
		}
	}
}

func TestClone_Apply_MissingSubdir(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Subdir: "missing"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error == nil {
		t.Fatal("Apply() expected an error for a subdir the tree does not have")
	}
}

func TestClone_Apply_UnknownRef(t *testing.T) {
	serveFixture(t)
	tmpDir := t.TempDir()

	a := &Clone{noShallow: true, ID: "clone", In: fixtureURL, Out: "src", Ref: "no-such-ref"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error == nil {
		t.Fatal("Apply() expected an error for an unknown ref")
	}
}

func TestClone_Validate(t *testing.T) {
	tests := []struct {
		name    string
		clone   *Clone
		wantErr bool
	}{
		{"valid", &Clone{ID: "c", In: "https://example.com/repo.git", Out: "src"}, false},
		{"valid with ref and subdir", &Clone{ID: "c", In: "https://example.com/repo.git", Out: "src", Ref: "main", Subdir: "app"}, false},
		{"missing in", &Clone{ID: "c", Out: "src"}, true},
		{"missing out", &Clone{ID: "c", In: "https://example.com/repo.git"}, true},
		{"credentials in url", &Clone{ID: "c", In: "https://user:secret@example.com/repo.git", Out: "src"}, true},
		{"non-http scheme", &Clone{ID: "c", In: "s3://bucket/repo.git", Out: "src"}, true},
		{"absolute out", &Clone{ID: "c", In: "https://example.com/repo.git", Out: "/src"}, true},
		{"traversal subdir", &Clone{ID: "c", In: "https://example.com/repo.git", Out: "src", Subdir: "../escape"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CloneDef.Validate("artifacts[0]", tt.clone)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIsCommitHash(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"6ecf0ef2c2dffb796033e5a02219af86ec6584e5", true},
		{"6ECF0EF2C2DFFB796033E5A02219AF86EC6584E5", true},
		{"main", false},
		{"", false},
		{"6ecf0ef2c2dffb796033e5a02219af86ec6584e", false},   // 39 chars
		{"6ecf0ef2c2dffb796033e5a02219af86ec6584e5a", false}, // 41 chars
		{"6ecf0ef2c2dffb796033e5a02219af86ec6584eg", false},  // non-hex
	}

	for _, tt := range tests {
		if got := isCommitHash(tt.ref); got != tt.want {
			t.Errorf("isCommitHash(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}
