package sidecar

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"orchestrator/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// objectStore stands in for S3: PUT stores, GET returns what was stored, and a
// key that was never written is a 404 — the case a first session hits.
type objectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    map[string]int
	fail    bool
	url     string
}

func newObjectStore(t *testing.T) *objectStore {
	t.Helper()
	s := &objectStore{objects: map[string][]byte{}, puts: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if s.fail {
				http.Error(w, "storage is having a day", http.StatusInternalServerError)
				return
			}
			body, _ := io.ReadAll(r.Body)
			s.objects[r.URL.Path] = body
			s.puts[r.URL.Path]++
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, ok := s.objects[r.URL.Path]
			if !ok {
				http.Error(w, "no such key", http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *objectStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects[key]) > 0
}

// writes counts successful uploads of a key — how many times a push actually
// left the pod.
func (s *objectStore) writes(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[key]
}

func (s *objectStore) put(key string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = body
}

// The delta is what the overlay's upper layer holds, and a push is an archive of
// exactly that directory — not the merged view, which would carry the whole
// image with it every time.
func TestPushDelta_ArchivesTheUpperLayerOnly(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	store := newObjectStore(t)

	// What a writable mount looks like on disk: the image's content is in the
	// lower layer, the workload's changes in the upper.
	upper := UpperDir(filepath.Join(ws, "work"))
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "work", ".lower"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "changed.txt"), "the delta")
	write(t, filepath.Join(ws, "work", ".lower", "from-image.txt"), "not the delta")

	r := NewRunner("t", ws, 30)
	m := &artifact.Mount{ID: "tree", In: "base.erofs", Out: "work", Writable: true, Sync: store.url + "/delta.tgz"}
	if err := r.pushDelta(t.Context(), m); err != nil {
		t.Fatalf("push: %v", err)
	}

	if !store.has("/delta.tgz") {
		t.Fatal("nothing was pushed")
	}
	// The staged archive must not be left behind in the workspace.
	if _, err := os.Stat(filepath.Join(ws, deltaArchivePath(m))); !os.IsNotExist(err) {
		t.Error("the staging archive should be cleaned up after the push")
	}
}

// A destination with nothing in it yet is a first session, not a failure.
func TestRestoreDelta_MissingDestinationIsAFirstSession(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	r := NewRunner("t", ws, 5)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true,
		Sync: newObjectStore(t).url + "/never-written.tgz"}

	if err := r.restoreDelta(t.Context(), m); err != nil {
		t.Fatalf("a first session must not fail: %v", err)
	}
	// And the upper layer exists, ready for the overlay to stack over it.
	if _, err := os.Stat(UpperDir(filepath.Join(ws, "work"))); err != nil {
		t.Errorf("upper layer should have been created: %v", err)
	}
}

// A destination that exists but cannot be read is NOT a first session. Starting
// empty would let the next push overwrite a workspace we merely failed to read.
func TestRestoreDelta_UnreadableDestinationFailsTheMount(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	store := newObjectStore(t)
	store.put("/corrupt.tgz", []byte("this is not an archive"))

	r := NewRunner("t", ws, 5)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/corrupt.tgz"}

	err := r.restoreDelta(t.Context(), m)
	if err == nil {
		t.Fatal("a delta that cannot be unpacked must fail the mount, not start empty")
	}
	if !strings.Contains(err.Error(), "delta") {
		t.Errorf("the error should name what failed, got %v", err)
	}
}

// Round trip: what one session pushes, the next restores.
func TestDelta_RoundTrips(t *testing.T) {
	t.Parallel()
	store := newObjectStore(t)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/session.tgz"}

	first := t.TempDir()
	upper := UpperDir(filepath.Join(first, "work"))
	if err := os.MkdirAll(filepath.Join(upper, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "nested", "notes.txt"), "session one")
	if err := NewRunner("a", first, 30).pushDelta(t.Context(), m); err != nil {
		t.Fatalf("push: %v", err)
	}

	second := t.TempDir()
	if err := NewRunner("b", second, 30).restoreDelta(t.Context(), m); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(UpperDir(filepath.Join(second, "work")), "nested/notes.txt"))
	if err != nil {
		t.Fatalf("the restored delta is missing: %v", err)
	}
	if string(got) != "session one" {
		t.Errorf("restored content: got %q", got)
	}
}

// A restored session must be able to change what it restored. The sidecar
// unpacks as root and the extraction does not trust the archive's modes, so
// without opening the tree up a resumed workspace is read-only to the workload —
// worse than a fresh one, and silently so.
func TestRestoreDelta_RestoredTreeIsWritableByTheWorkload(t *testing.T) {
	t.Parallel()
	store := newObjectStore(t)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/modes.tgz"}

	first := t.TempDir()
	upper := UpperDir(filepath.Join(first, "work"))
	if err := os.MkdirAll(filepath.Join(upper, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "dir", "notes.txt"), "session one")
	write(t, filepath.Join(upper, "run.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(upper, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewRunner("a", first, 30).pushDelta(t.Context(), m); err != nil {
		t.Fatalf("push: %v", err)
	}

	second := t.TempDir()
	if err := NewRunner("b", second, 30).restoreDelta(t.Context(), m); err != nil {
		t.Fatalf("restore: %v", err)
	}

	root := UpperDir(filepath.Join(second, "work"))
	for path, want := range map[string]os.FileMode{
		filepath.Join(root, "dir"):              0o777,
		filepath.Join(root, "dir", "notes.txt"): 0o666,
		filepath.Join(root, "run.sh"):           0o777, // 0o666 plus the execute bit
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
		}
	}
}

func TestMakeWritable_DoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	write(t, outside, "leave permissions alone")
	if err := os.Chmod(outside, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	if err := makeWritable(root); err != nil {
		t.Fatalf("makeWritable: %v", err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("external symlink target mode = %o, want 600", got)
	}
}

// A change that moves no bytes is still a change. An empty mkdir and a rename
// both leave the file count, the total size and every file mtime exactly as they
// were, so a fingerprint over files alone would call them unchanged and skip the
// push — losing them until something else happened to be written.
func TestPushDelta_NoticesChangesThatMoveNoBytes(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	store := newObjectStore(t)
	upper := UpperDir(filepath.Join(ws, "work"))
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "a.txt"), "one")

	r := NewRunner("t", ws, 30)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/delta.tgz"}
	if err := r.pushDelta(t.Context(), m); err != nil {
		t.Fatalf("first push: %v", err)
	}

	for _, tc := range []struct {
		name   string
		change func() error
	}{
		{"an empty directory", func() error {
			return os.Mkdir(filepath.Join(upper, "empty"), 0o755)
		}},
		// A rename keeps the size and the mtime, so only the containing
		// directory's mtime gives it away.
		{"a rename", func() error {
			return os.Rename(filepath.Join(upper, "a.txt"), filepath.Join(upper, "b.txt"))
		}},
	} {
		before := store.writes("/delta.tgz")
		if err := tc.change(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := r.pushDelta(t.Context(), m); err != nil {
			t.Fatalf("push after %s: %v", tc.name, err)
		}
		if got := store.writes("/delta.tgz"); got != before+1 {
			t.Errorf("%s must be pushed: %d uploads, want %d", tc.name, got, before+1)
		}
	}
}

// Stopping flushes: a workload torn down normally loses nothing, which is what
// makes the interval the bound on what a crash can cost.
func TestStopSync_FlushesOnTheWayOut(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	store := newObjectStore(t)
	upper := UpperDir(filepath.Join(ws, "work"))
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRunner("t", ws, 30)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/delta.tgz",
		SyncIntervalSeconds: 3600} // long enough that only the flush can have run
	r.startSync(m)

	write(t, filepath.Join(upper, "late.txt"), "written after the last tick")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r.StopSync(ctx)

	if !store.has("/delta.tgz") {
		t.Fatal("teardown must flush the delta")
	}
	// Idempotent: a second stop is a no-op, not a second flush or a panic.
	r.StopSync(ctx)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A short interval is only affordable if an idle workload costs nothing: a push
// with no changes since the last successful one must not upload anything.
func TestPushDelta_SkipsWhenNothingChanged(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	store := newObjectStore(t)
	upper := UpperDir(filepath.Join(ws, "work"))
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "a.txt"), "one")

	r := NewRunner("t", ws, 30)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/delta.tgz"}

	if err := r.pushDelta(t.Context(), m); err != nil {
		t.Fatalf("first push: %v", err)
	}
	first := store.writes("/delta.tgz")
	if first != 1 {
		t.Fatalf("first push should have uploaded once, got %d", first)
	}

	// Nothing touched the tree: no upload.
	for range 3 {
		if err := r.pushDelta(t.Context(), m); err != nil {
			t.Fatalf("idle push: %v", err)
		}
	}
	if got := store.writes("/delta.tgz"); got != first {
		t.Errorf("an idle workload must not upload: %d uploads, want %d", got, first)
	}

	// A change is picked up.
	write(t, filepath.Join(upper, "b.txt"), "two")
	if err := r.pushDelta(t.Context(), m); err != nil {
		t.Fatalf("push after change: %v", err)
	}
	if got := store.writes("/delta.tgz"); got != first+1 {
		t.Errorf("a change must be pushed: %d uploads, want %d", got, first+1)
	}

	// So must a deletion, which the overlay records as a whiteout in the upper.
	if err := os.Remove(filepath.Join(upper, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := r.pushDelta(t.Context(), m); err != nil {
		t.Fatalf("push after delete: %v", err)
	}
	if got := store.writes("/delta.tgz"); got != first+2 {
		t.Errorf("a deletion must be pushed: %d uploads, want %d", got, first+2)
	}
}

// A failed push must not be remembered as the baseline, or the next tick would
// decide there was nothing to do and the change would never leave the pod.
func TestPushDelta_FailedPushIsRetried(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	store := newObjectStore(t)
	store.fail = true
	upper := UpperDir(filepath.Join(ws, "work"))
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "a.txt"), "one")

	r := NewRunner("t", ws, 5)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/delta.tgz"}

	if err := r.pushDelta(t.Context(), m); err == nil {
		t.Fatal("want the push to fail")
	}
	store.fail = false
	if err := r.pushDelta(t.Context(), m); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !store.has("/delta.tgz") {
		t.Error("the retry should have uploaded what the failure did not")
	}
}

// Restoring records the baseline too, so a session that changes nothing never
// re-uploads what it just downloaded.
func TestRestoreDelta_MakesTheRestoredTreeTheBaseline(t *testing.T) {
	t.Parallel()
	store := newObjectStore(t)
	m := &artifact.Mount{ID: "tree", Out: "work", Writable: true, Sync: store.url + "/session.tgz"}

	first := t.TempDir()
	upper := UpperDir(filepath.Join(first, "work"))
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "notes.txt"), "session one")
	if err := NewRunner("a", first, 30).pushDelta(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	uploads := store.writes("/session.tgz")

	second := t.TempDir()
	r := NewRunner("b", second, 30)
	if err := r.restoreDelta(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if err := r.pushDelta(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if got := store.writes("/session.tgz"); got != uploads {
		t.Errorf("a session that changed nothing must not push: %d uploads, want %d", got, uploads)
	}
}

// RunPost must preserve saved edits, keep syncing while the worker runs, and
// flush its last edit before unmounting, including after a sidecar restart.
func TestRunPost_SyncedMount(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		name := "fresh"
		if adopted {
			name = "adopted"
		}
		t.Run(name, func(t *testing.T) {
			store := newObjectStore(t)
			seed := filepath.Join(t.TempDir(), "saved.tgz")
			createTarFile(t, seed, true, map[string]string{"notes.txt": "saved"})
			body, err := os.ReadFile(seed)
			if err != nil {
				t.Fatal(err)
			}
			store.put("/delta.tgz", body)
			ws := t.TempDir()
			target := filepath.Join(ws, "work")
			upper := UpperDir(target)
			createTarFile(t, filepath.Join(ws, "base.tgz"), true, map[string]string{"base.txt": "base"})
			expected := "saved"
			if adopted {
				if err := os.MkdirAll(upper, 0o755); err != nil {
					t.Fatal(err)
				}
				expected = "local newer than saved"
				write(t, filepath.Join(upper, "notes.txt"), expected)
			}
			m := &artifact.Mount{ID: "tree", In: "base.tgz", Out: "work", Writable: true,
				Sync: store.url + "/delta.tgz", SyncIntervalSeconds: 1}
			fake := &fakeMounter{active: map[string]bool{target: adopted}}
			r := NewRunner("post", ws, 10, WithMounter(fake), WithSignalFunc(func(context.Context) {
				got, err := os.ReadFile(filepath.Join(upper, "notes.txt"))
				if err != nil || string(got) != expected {
					t.Fatalf("worker sees %q (%v), want %q", got, err, expected)
				}
				write(t, filepath.Join(upper, "notes.txt"), "periodic edit")
				testutil.MustWaitFor(t, func() bool { return store.writes("/delta.tgz") > 0 }, testutil.WithTimeout(5*time.Second))
				write(t, filepath.Join(upper, "notes.txt"), "final edit")
			}))
			if err := r.RunPost(t.Context(), []artifact.Artifact{m}); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			saved := append([]byte(nil), store.objects["/delta.tgz"]...)
			store.mu.Unlock()
			check := t.TempDir()
			if err := os.WriteFile(filepath.Join(check, "saved.tgz"), saved, 0o600); err != nil {
				t.Fatal(err)
			}
			result := (&artifact.Unarchive{ID: "check", In: "saved.tgz", Out: "restored"}).Apply(t.Context(), check)
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			got, err := os.ReadFile(filepath.Join(check, "restored", "notes.txt"))
			if err != nil || string(got) != "final edit" {
				t.Fatalf("persisted %q (%v), want final edit", got, err)
			}
			mounted, err := fake.IsMounted(target)
			if err != nil || mounted {
				t.Fatalf("mount remains after shutdown: %v (%v)", mounted, err)
			}
		})
	}
}
