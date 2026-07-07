package artifact

import (
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock(t *testing.T) {
	t.Helper()
	prev := s3Now
	s3Now = func() time.Time { return time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC) }
	t.Cleanup(func() { s3Now = prev })
}

var testCreds = config.S3Credentials{
	Region:          "us-east-1",
	AccessKeyID:     "AKIDEXAMPLE",
	SecretAccessKey: "secret",
}

func TestNewSignedS3Request_VirtualHosted(t *testing.T) {
	fixedClock(t)
	req, err := newSignedS3Request(t.Context(), http.MethodGet, "s3://mybucket/path/to/obj.txt", nil, 0, testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.String(), "https://mybucket.s3.us-east-1.amazonaws.com/path/to/obj.txt"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if req.Host != "mybucket.s3.us-east-1.amazonaws.com" {
		t.Errorf("Host = %q", req.Host)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential=AKIDEXAMPLE/20230102/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization missing/incorrect credential scope: %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("Authorization signed headers wrong: %q", auth)
	}
}

func TestNewSignedS3Request_PathStyleEndpoint(t *testing.T) {
	fixedClock(t)
	creds := testCreds
	creds.Endpoint = "http://minio:9000"
	req, err := newSignedS3Request(t.Context(), http.MethodPut, "s3://mybucket/obj", nil, 0, creds)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.String(), "http://minio:9000/mybucket/obj"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if req.Host != "minio:9000" {
		t.Errorf("Host = %q, want minio:9000", req.Host)
	}
}

func TestNewSignedS3Request_EndpointPathPrefix(t *testing.T) {
	fixedClock(t)
	creds := testCreds
	creds.Endpoint = "http://gw:9000/s3"
	req, err := newSignedS3Request(t.Context(), http.MethodGet, "s3://mybucket/obj", nil, 0, creds)
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint's /s3 prefix must survive into the target URL...
	if got, want := req.URL.String(), "http://gw:9000/s3/mybucket/obj"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	// ...and into the signed canonical URI (else the gateway rejects the signature).
	if got := req.URL.EscapedPath(); got != "/s3/mybucket/obj" {
		t.Errorf("path = %q, want /s3/mybucket/obj", got)
	}
}

func TestNewSignedS3Request_Deterministic(t *testing.T) {
	fixedClock(t)
	a, err := newSignedS3Request(t.Context(), http.MethodGet, "s3://b/k", nil, 0, testCreds)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newSignedS3Request(t.Context(), http.MethodGet, "s3://b/k", nil, 0, testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if a.Header.Get("Authorization") != b.Header.Get("Authorization") {
		t.Error("signature is not deterministic for identical inputs")
	}
}

func TestNewSignedS3Request_NoCredentials(t *testing.T) {
	_, err := newSignedS3Request(t.Context(), http.MethodGet, "s3://b/k", nil, 0, config.S3Credentials{})
	if err == nil {
		t.Fatal("expected error when credentials are not configured")
	}
}

func TestNewSignedS3Request_BadURL(t *testing.T) {
	if _, err := newSignedS3Request(t.Context(), http.MethodGet, "s3://bucket", nil, 0, testCreds); err == nil {
		t.Error("expected error for s3 URL with no key")
	}
}

func TestUriEncodePath(t *testing.T) {
	cases := map[string]string{
		"path/to/obj":   "path/to/obj",
		"a b+c":         "a%20b%2Bc",
		"tilde~and.dot": "tilde~and.dot",
	}
	for in, want := range cases {
		if got := uriEncodePath(in); got != want {
			t.Errorf("uriEncodePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDownload_ApplyS3 exercises the s3:// branch end to end against a test
// server acting as the object store, asserting the request is signed.
func TestDownload_ApplyS3(t *testing.T) {
	const content = "s3 object body"
	var gotAuth, gotPath, gotContentHash string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotContentHash = r.Header.Get("X-Amz-Content-Sha256")
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	creds := testCreds
	creds.Endpoint = server.URL

	tmp := t.TempDir()
	a := &Download{ID: "d", In: "s3://bucket/dir/key.txt", Out: "out.txt"}
	a.SetS3Credentials(creds)
	if res := a.Apply(t.Context(), tmp); res.Error != nil {
		t.Fatalf("Apply() error = %v", res.Error)
	}

	if gotPath != "/bucket/dir/key.txt" {
		t.Errorf("request path = %q, want /bucket/dir/key.txt", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("missing SigV4 Authorization header: %q", gotAuth)
	}
	if gotContentHash != emptyPayloadHash {
		t.Errorf("X-Amz-Content-Sha256 = %q, want empty payload hash", gotContentHash)
	}
	got, err := os.ReadFile(filepath.Join(tmp, "out.txt"))
	if err != nil || string(got) != content {
		t.Errorf("downloaded content = %q (err %v), want %q", got, err, content)
	}
}

// TestDownload_ApplyS3_NoCreds confirms an s3:// download without configured
// credentials fails clearly instead of sending an unsigned request.
func TestDownload_ApplyS3_NoCreds(t *testing.T) {
	a := &Download{ID: "d", In: "s3://bucket/key", Out: "out.txt"}
	res := a.Apply(t.Context(), t.TempDir())
	if res.Error == nil {
		t.Fatal("expected failure without S3 credentials")
	}
}

// TestUpload_ApplyS3 exercises the s3:// upload branch: the file body reaches
// the store via a signed PUT with the body's content hash.
func TestUpload_ApplyS3(t *testing.T) {
	const content = "file to upload"
	var gotBody, gotAuth, gotMethod, gotHash string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotHash = r.Header.Get("X-Amz-Content-Sha256")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "in.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	creds := testCreds
	creds.Endpoint = server.URL

	a := &Upload{ID: "u", In: "in.txt", Out: "s3://bucket/key.txt"}
	a.SetS3Credentials(creds)
	if res := a.Apply(t.Context(), tmp); res.Error != nil {
		t.Fatalf("Apply() error = %v", res.Error)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotBody != content {
		t.Errorf("uploaded body = %q, want %q", gotBody, content)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("missing SigV4 Authorization header: %q", gotAuth)
	}
	if gotHash == "" || gotHash == emptyPayloadHash {
		t.Errorf("X-Amz-Content-Sha256 = %q, want the file's content hash", gotHash)
	}
}

// TestUpload_ApplyS3_RetryRehashesBody guards the ReadSeeker rewind: a retried
// s3:// upload must re-read the body, re-hash it, and resend the full content —
// not send an empty or truncated body on the second attempt.
func TestUpload_ApplyS3_RetryRehashesBody(t *testing.T) {
	const content = "body that must survive a retry"
	var attempts int
	var lastBody, lastHash string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		lastHash = r.Header.Get("X-Amz-Content-Sha256")
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError) // force one retry
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "in.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := testCreds
	creds.Endpoint = server.URL

	a := &Upload{ID: "u", In: "in.txt", Out: "s3://bucket/key.txt"}
	a.SetS3Credentials(creds)
	if res := a.Apply(t.Context(), tmp); res.Error != nil {
		t.Fatalf("Apply() error = %v", res.Error)
	}

	if attempts < 2 {
		t.Fatalf("expected a retry, got %d attempt(s)", attempts)
	}
	if lastBody != content {
		t.Errorf("retried body = %q, want %q", lastBody, content)
	}
	if lastHash == "" || lastHash == emptyPayloadHash {
		t.Errorf("retried X-Amz-Content-Sha256 = %q, want the file's content hash", lastHash)
	}
}
