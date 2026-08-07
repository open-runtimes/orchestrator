package sidecar

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
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
	url     string
}

func newObjectStore(t *testing.T) *objectStore {
	t.Helper()
	s := &objectStore{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			s.objects[r.URL.Path] = body
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
	if err := os.MkdirAll(filepath.Join(ws, "work.lower"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(upper, "changed.txt"), "the delta")
	write(t, filepath.Join(ws, "work.lower", "from-image.txt"), "not the delta")

	r := NewRunner("t", ws, 30, artifact.DefaultRegistry())
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
	r := NewRunner("t", ws, 5, artifact.DefaultRegistry())
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

	r := NewRunner("t", ws, 5, artifact.DefaultRegistry())
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
	if err := NewRunner("a", first, 30, artifact.DefaultRegistry()).pushDelta(t.Context(), m); err != nil {
		t.Fatalf("push: %v", err)
	}

	second := t.TempDir()
	if err := NewRunner("b", second, 30, artifact.DefaultRegistry()).restoreDelta(t.Context(), m); err != nil {
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

	r := NewRunner("t", ws, 30, artifact.DefaultRegistry())
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
