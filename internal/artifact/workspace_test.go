package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// secret is what sits outside the workspace. No artifact may read it, write
// next to it, or carry it anywhere.
const secret = "kept-outside-the-workspace"

// confinedWorkspace lays out a workspace next to a directory it must never
// reach, plus the links a workload could leave behind in the workspace before
// a post-phase artifact runs. It returns the workspace and the outside dir.
func confinedWorkspace(t *testing.T) (ws, outside string) {
	t.Helper()
	base := t.TempDir()
	ws = filepath.Join(base, "ws")
	outside = filepath.Join(base, "outside")
	for _, dir := range []string{ws, outside, filepath.Join(ws, "inside")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "inside", "file"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"abs-link":     filepath.Join(outside, "secret"),
		"rel-link":     "../outside/secret",
		"abs-dir-link": outside,
		"rel-dir-link": "../outside",
		"in-link":      "inside/file",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	return ws, outside
}

// assertOutsideUntouched fails when anything beyond the planted secret exists
// outside the workspace, or the secret changed.
func assertOutsideUntouched(t *testing.T, outside string) {
	t.Helper()
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("outside the workspace now holds %v, want only [secret]", names)
	}
	content, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(content) != secret {
		t.Errorf("outside secret = %q, %v; want it unchanged", content, err)
	}
}

// assertNotCopiedIn fails when the secret's bytes landed in any regular file
// inside the workspace.
func assertNotCopiedIn(t *testing.T, ws string) {
	t.Helper()
	_ = filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // keep walking: an unreadable entry cannot hold the content
		}
		if !d.Type().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(p)
		if err == nil && bytes.Contains(content, []byte(secret)) {
			t.Errorf("%s holds the content from outside the workspace", p)
		}
		return nil
	})
}

// escapingReads are names that point at the outside secret, through every
// route a name can take: climbing out, an absolute name, and links.
func escapingReads(outside string) []string {
	return []string{
		"../outside/secret",
		filepath.Join(outside, "secret"),
		"abs-link",
		"rel-link",
		"abs-dir-link/secret",
		"rel-dir-link/secret",
		"inside/../../outside/secret",
	}
}

// escapingWrites are names whose target would land outside the workspace.
func escapingWrites(outside string) []string {
	return []string{
		"../outside/new",
		filepath.Join(outside, "new"),
		"abs-dir-link/new",
		"rel-dir-link/new",
		"abs-link",
		"rel-link",
	}
}

func TestReadersStayInsideWorkspace(t *testing.T) {
	ws, outside := confinedWorkspace(t)

	var mu sync.Mutex
	var uploaded bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.Copy(&uploaded, r.Body)
	}))
	defer server.Close()

	for _, name := range escapingReads(outside) {
		artifacts := []Artifact{
			&Read{ID: "read", In: name},
			&Stat{ID: "stat", In: name},
			&List{ID: "list", In: name},
			&Upload{ID: "upload", In: name, Out: server.URL, Retries: 1},
			&Archive{ID: "tar", In: name, Out: "out.tar", Format: "tar"},
			&Archive{ID: "erofs", In: name, Out: "out.erofs", Format: "erofs"},
			&Unarchive{ID: "unarchive", In: name, Out: "extracted"},
		}
		for _, a := range artifacts {
			t.Run(a.ArtifactID()+"/"+name, func(t *testing.T) {
				result := a.Apply(context.Background(), ws)
				if strings.Contains(stringContent(result.Content), secret) {
					t.Fatal("Apply() returned the content from outside the workspace")
				}
				// A top-level link is archived as a link, not followed; that
				// stays inside. Everything else has to fail.
				if a.ArtifactID() != "tar" && result.Status != "failed" {
					t.Errorf("Apply(%q) status = %q, want failed", name, result.Status)
				}
			})
		}
	}

	mu.Lock()
	if bytes.Contains(uploaded.Bytes(), []byte(secret)) {
		t.Error("an upload carried the content from outside the workspace")
	}
	mu.Unlock()
	assertNotCopiedIn(t, ws)
	assertOutsideUntouched(t, outside)
}

func TestWritersStayInsideWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("downloaded"))
	}))
	defer server.Close()

	_, outsideNames := confinedWorkspace(t)
	for _, name := range escapingWrites(outsideNames) {
		artifacts := []func(outside string) Artifact{
			func(outside string) Artifact {
				return &Write{ID: "write", In: "written", Out: rebase(name, outsideNames, outside)}
			},
			func(outside string) Artifact {
				return &Download{ID: "download", In: server.URL, Out: rebase(name, outsideNames, outside)}
			},
			func(outside string) Artifact {
				return &Archive{ID: "tar", In: "inside", Out: rebase(name, outsideNames, outside), Format: "tar"}
			},
			func(outside string) Artifact {
				return &Archive{ID: "squashfs", In: "inside", Out: rebase(name, outsideNames, outside), Format: "squashfs"}
			},
			func(outside string) Artifact {
				return &Archive{ID: "erofs", In: "inside", Out: rebase(name, outsideNames, outside), Format: "erofs"}
			},
			func(outside string) Artifact {
				return &Unarchive{ID: "unarchive", In: "in.tar", Out: rebase(name, outsideNames, outside)}
			},
			// Never reachable: the destination is refused before any fetch.
			func(outside string) Artifact {
				return &Clone{ID: "clone", In: "http://127.0.0.1:1/repo.git", Out: rebase(name, outsideNames, outside), TimeoutSeconds: 1}
			},
		}
		for _, build := range artifacts {
			ws, outside := confinedWorkspace(t)
			writeTar(t, filepath.Join(ws, "in.tar"), map[string]string{"file": "archived"})
			a := build(outside)
			t.Run(a.ArtifactID()+"/"+rebase(name, outsideNames, "<outside>"), func(t *testing.T) {
				result := a.Apply(context.Background(), ws)
				// Download renames over a leaf link instead of following it,
				// which replaces the link inside the workspace.
				if a.ArtifactID() == "download" && (name == "abs-link" || name == "rel-link") {
					if result.Status != "success" {
						t.Errorf("Apply(%q) = %v, want the link replaced", name, result.Error)
					}
				} else if result.Status != "failed" {
					t.Errorf("Apply(%q) status = %q, want failed", name, result.Status)
				}
				assertOutsideUntouched(t, outside)
			})
		}
	}
}

// rebase moves an absolute name from one confined workspace's outside dir to
// another's, so every case gets a fresh layout.
func rebase(name, from, to string) string {
	if rest, ok := strings.CutPrefix(name, from); ok {
		return to + rest
	}
	return name
}

// An image extracted into a destination that already holds a link must write
// beside the link, not through it.
func TestImageExtractionDoesNotFollowLinksInDestination(t *testing.T) {
	ws, outside := confinedWorkspace(t)

	if err := os.MkdirAll(filepath.Join(ws, "tree", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tree", "sub", "new"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"squashfs", "erofs"} {
		t.Run(format, func(t *testing.T) {
			image := "tree." + format
			if result := (&Archive{ID: "a", In: "tree", Out: image, Format: format}).Apply(context.Background(), ws); result.Status != "success" {
				t.Fatalf("Archive() = %v", result.Error)
			}
			dest := filepath.Join(ws, "dest-"+format)
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dest, "sub")); err != nil {
				t.Fatal(err)
			}

			result := (&Unarchive{ID: "u", In: image, Out: "dest-" + format}).Apply(context.Background(), ws)
			if result.Status != "failed" {
				t.Errorf("Unarchive() status = %q, want failed", result.Status)
			}
			assertOutsideUntouched(t, outside)
		})
	}
}

// Links that stay inside the workspace keep working.
func TestLinksInsideWorkspaceStillResolve(t *testing.T) {
	ws, _ := confinedWorkspace(t)

	result := (&Read{ID: "r", In: "in-link"}).Apply(context.Background(), ws)
	if result.Status != "success" || result.Content != "inside" {
		t.Fatalf("Read(in-link) = %v %v, %v; want the linked file", result.Status, result.Content, result.Error)
	}

	if err := os.Symlink("inside", filepath.Join(ws, "inside-dir-link")); err != nil {
		t.Fatal(err)
	}
	result = (&Write{ID: "w", In: "through", Out: "inside-dir-link/new"}).Apply(context.Background(), ws)
	if result.Status != "success" {
		t.Fatalf("Write(inside-dir-link/new) = %v", result.Error)
	}
	content, err := os.ReadFile(filepath.Join(ws, "inside", "new"))
	if err != nil || string(content) != "through" {
		t.Fatalf("inside/new = %q, %v; want the written content", content, err)
	}
}

func stringContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []string:
		return strings.Join(c, "\n")
	}
	return ""
}

func writeTar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}
