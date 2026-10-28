package artifact

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrantsFor(t *testing.T) {
	archive := testCreds
	archive.SecretAccessKey = "archive-secret"
	profiles := config.S3Profiles{"": testCreds, "archive": archive}

	g := GrantsFor([]Artifact{
		&Download{ID: "in", In: "s3://b/in.tar"},
		&Download{ID: "web", In: "https://example.com/in.tar"},
		&Upload{ID: "out", Out: "s3://archive@b/out.tar"},
		&Upload{ID: "lost", Out: "s3://missing@b/out.tar"},
		&Mount{ID: "m", Sync: "s3://b/delta.tgz"},
		&Mount{ID: "plain"},
	}, profiles)

	want := []string{"GET s3://b/in.tar", "PUT s3://archive@b/out.tar", "GET s3://b/delta.tgz", "PUT s3://b/delta.tgz"}
	if len(g) != len(want) {
		t.Fatalf("grants = %v, want exactly %v", g, want)
	}
	for _, k := range want {
		if g[k] == "" {
			t.Errorf("no grant for %q", k)
		}
	}
	if g["GET s3://b/delta.tgz"] == g["PUT s3://b/delta.tgz"] {
		t.Error("a read grant must not also be the write grant")
	}
	if GrantsFor([]Artifact{&Download{In: "https://x/y"}}, profiles) != nil {
		t.Error("no s3 transfers must give no grants")
	}
}

func TestGrants_JSONRoundTrip(t *testing.T) {
	g := Grants{"GET s3://b/k": "abc"}
	if got := ParseGrants(g.JSON()); got["GET s3://b/k"] != "abc" || len(got) != 1 {
		t.Errorf("round trip = %v", got)
	}
	if Grants(nil).JSON() != "" || ParseGrants("") != nil || ParseGrants("{not json") != nil {
		t.Error("empty and malformed grants must come back empty")
	}
}

// redeem posts to the handler the way GrantSigner does and returns the status.
func redeem(t *testing.T, h http.Handler, method, rawURL, grant string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(presignRequest{Method: method, URL: rawURL, Grant: grant})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, PresignPath, bytes.NewReader(body)))
	return rec
}

func TestPresignHandler_RedeemsOnlyWhatWasGranted(t *testing.T) {
	other := testCreds
	other.SecretAccessKey = "other"
	profiles := config.S3Profiles{"": testCreds, "other": other}
	h := PresignHandler(profiles)
	g := GrantsFor([]Artifact{&Download{In: "s3://b/k"}, &Upload{Out: "s3://other@b/k"}}, profiles)

	rec := redeem(t, h, http.MethodGet, "s3://b/k", g["GET s3://b/k"])
	if rec.Code != http.StatusOK {
		t.Fatalf("valid grant: status %d: %s", rec.Code, rec.Body)
	}
	var out presignResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !strings.Contains(out.URL, "X-Amz-Signature=") {
		t.Fatalf("response = %s", rec.Body)
	}

	for name, tc := range map[string]struct{ method, url, grant string }{
		"read grant used to write": {http.MethodPut, "s3://b/k", g["GET s3://b/k"]},
		"grant for another object": {http.MethodGet, "s3://b/other", g["GET s3://b/k"]},
		"grant moved to a profile": {http.MethodPut, "s3://b/k", g["PUT s3://other@b/k"]},
		"no grant":                 {http.MethodGet, "s3://b/k", ""},
	} {
		if rec := redeem(t, h, tc.method, tc.url, tc.grant); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}
	for name, tc := range map[string]struct{ method, url string }{
		"not s3":     {http.MethodGet, "https://example.com/k"},
		"not a read": {http.MethodDelete, "s3://b/k"},
	} {
		if rec := redeem(t, h, tc.method, tc.url, "x"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
}

// objectStore stands in for S3: it records each request and serves or stores
// bodies by path. failFirst fails the first request with a 500.
type objectStore struct {
	objects   map[string]string
	requests  []*http.Request
	failFirst bool
}

func (s *objectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests = append(s.requests, r)
	if s.failFirst && len(s.requests) == 1 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		body, ok := s.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		s.objects[r.URL.Path] = string(b)
	}
}

// grantedTransfer wires a pod's view (a GrantSigner holding only grants) to a
// service (PresignHandler holding the keys) that signs for the object store.
func grantedTransfer(t *testing.T, store *objectStore, artifacts ...Artifact) *GrantSigner {
	t.Helper()
	objects := httptest.NewServer(store)
	t.Cleanup(objects.Close)
	creds := testCreds
	creds.Endpoint = objects.URL
	profiles := config.S3Profiles{"": creds}
	service := httptest.NewServer(PresignHandler(profiles))
	t.Cleanup(service.Close)
	return &GrantSigner{Endpoint: service.URL, Grants: GrantsFor(artifacts, profiles)}
}

func TestDownload_S3ThroughGrant(t *testing.T) {
	store := &objectStore{objects: map[string]string{"/bucket/dir/key.txt": "s3 object body"}}
	a := &Download{ID: "d", In: "s3://bucket/dir/key.txt", Out: "out.txt"}
	a.SetSigner(grantedTransfer(t, store, a))

	tmp := t.TempDir()
	if res := a.Apply(t.Context(), tmp); res.Error != nil {
		t.Fatalf("Apply() error = %v", res.Error)
	}
	if got, err := os.ReadFile(filepath.Join(tmp, "out.txt")); err != nil || string(got) != "s3 object body" {
		t.Errorf("downloaded %q (err %v)", got, err)
	}
	r := store.requests[0]
	if r.URL.Query().Get("X-Amz-Signature") == "" || r.Header.Get("Authorization") != "" {
		t.Errorf("store saw %s with Authorization %q, want a pre-signed URL", r.URL, r.Header.Get("Authorization"))
	}
}

// A retried upload sends the whole body again, on a freshly signed URL.
func TestUpload_S3ThroughGrant_Retry(t *testing.T) {
	store := &objectStore{objects: map[string]string{}, failFirst: true}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "in.txt"), []byte("body that must survive a retry"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Upload{ID: "u", In: "in.txt", Out: "s3://bucket/key.txt"}
	a.SetSigner(grantedTransfer(t, store, a))

	if res := a.Apply(t.Context(), tmp); res.Error != nil {
		t.Fatalf("Apply() error = %v", res.Error)
	}
	if len(store.requests) < 2 {
		t.Fatalf("expected a retry, got %d request(s)", len(store.requests))
	}
	if got := store.objects["/bucket/key.txt"]; got != "body that must survive a retry" {
		t.Errorf("stored %q", got)
	}
}

// Without a grant for the exact transfer the pod cannot reach the object,
// even with a signer and an endpoint.
func TestDownload_S3WithoutGrantFails(t *testing.T) {
	store := &objectStore{objects: map[string]string{"/bucket/secret": "x"}}
	granted := &Download{ID: "ok", In: "s3://bucket/ok", Out: "ok"}
	signer := grantedTransfer(t, store, granted)

	a := &Download{ID: "d", In: "s3://bucket/secret", Out: "out"}
	a.SetSigner(signer)
	if res := a.Apply(t.Context(), t.TempDir()); res.Error == nil {
		t.Fatal("a transfer with no grant must fail")
	}
	if len(store.requests) != 0 {
		t.Errorf("store saw %d request(s), want none", len(store.requests))
	}

	bare := &Download{ID: "d", In: "s3://bucket/ok", Out: "out"}
	if res := bare.Apply(t.Context(), t.TempDir()); res.Error == nil {
		t.Fatal("an s3 download with no signer must fail")
	}
}
