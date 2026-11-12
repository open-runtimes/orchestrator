package sidecar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"orchestrator/internal/mount"
	"orchestrator/internal/mountagent"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeFetcher is a node agent with a cache: it records each fetch-mount, or
// refuses them all with err.
type fakeFetcher struct {
	fakeMounter
	err     error
	fetched []string // url -> target
	opts    []mount.Opts
}

func (f *fakeFetcher) FetchMount(url, target string, opts mount.Opts) (mountagent.FetchMountResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return mountagent.FetchMountResponse{}, f.err
	}
	f.fetched = append(f.fetched, url+" -> "+target)
	f.opts = append(f.opts, opts)
	if f.active == nil {
		f.active = map[string]bool{}
	}
	f.active[target] = true
	return mountagent.FetchMountResponse{Format: "erofs"}, nil
}

// objectServer serves every path as a squashfs-looking file and records what
// was downloaded.
func objectServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte("hsqs image"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

type signerFunc func(ctx context.Context, method, rawURL string) (string, error)

func (f signerFunc) Sign(ctx context.Context, method, rawURL string) (string, error) {
	return f(ctx, method, rawURL)
}

// runCombined runs the combined flow until the workload would be admitted, then
// finishes it.
func runCombined(t *testing.T, workspace string, m Mounter, arts []artifact.Artifact, opts ...Option) *captureReporter {
	t.Helper()
	captured := &captureReporter{}
	sig, finish := triggerSignal()
	opts = append(opts, WithSignalFunc(sig), WithMounter(m), WithArtifactListener(captured.fn()))
	runner := NewRunner("job", workspace, 10, opts...)
	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context(), arts) }()
	for deadline := time.Now().Add(3 * time.Second); !CheckReady(workspace); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("workload never admitted")
		}
	}
	finish()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return captured
}

// edgeArtifacts is a runtime's shape: a source image downloaded and mounted
// writable, plus a file the workload reads.
func edgeArtifacts(source, env string) []artifact.Artifact {
	return []artifact.Artifact{
		&artifact.Download{ID: "source", In: source, Out: "source.erofs"},
		&artifact.Mount{ID: "code", In: "source.erofs", Out: "code", Writable: true, Size: 64, Depends: "source"},
		&artifact.Download{ID: "env", In: env, Out: "env/vars"},
	}
}

// With an agent that caches, the sidecar does not download a mounted image: it
// hands the agent a URL it may GET (an s3:// one pre-signed with its grant) and
// the agent mounts it.
func TestRunner_FetchesMountedDownloadsThroughTheAgent(t *testing.T) {
	srv, downloaded := objectServer(t)
	workspace := t.TempDir()
	agent := &fakeFetcher{}
	signer := signerFunc(func(_ context.Context, method, rawURL string) (string, error) {
		return srv.URL + "/presigned?" + method + "=" + rawURL, nil
	})

	captured := runCombined(t, workspace, agent, edgeArtifacts("s3://builds/a.erofs", srv.URL+"/env"), WithSigner(signer))

	if want := []string{srv.URL + "/presigned?GET=s3://builds/a.erofs -> code"}; !slices.Equal(agent.fetched, want) {
		t.Fatalf("fetch-mounts = %v, want %v", agent.fetched, want)
	}
	if o := agent.opts[0]; !o.Writable || o.SizeMiB != 64 {
		t.Fatalf("fetch-mount opts = %+v, want writable with 64 MiB", o)
	}
	if got := downloaded(); !slices.Equal(got, []string{"/env"}) {
		t.Fatalf("sidecar downloaded %v, want only the unmounted file", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, "source.erofs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the image was written to the workspace: %v", err)
	}
	if len(agent.mounted) != 0 {
		t.Fatalf("also mounted from the workspace: %v", agent.mounted)
	}
	for _, id := range []string{"source", "code", "env"} {
		if !slices.ContainsFunc(captured.reports, func(r job.ArtifactReport) bool { return r.ID == id && r.Status == "success" }) {
			t.Errorf("no successful report for %s: %+v", id, captured.reports)
		}
	}
}

// An agent that will not serve the image (no cache, too large, not an image)
// leaves the sidecar to download and mount it as it would without a cache.
func TestRunner_DownloadsWhenTheAgentDoesNotCache(t *testing.T) {
	srv, downloaded := objectServer(t)
	workspace := t.TempDir()
	agent := &fakeFetcher{err: errors.New("mount agent: 404 Not Found")}

	runCombined(t, workspace, agent, edgeArtifacts(srv.URL+"/a.erofs", srv.URL+"/env"))

	if got := downloaded(); !slices.Contains(got, "/a.erofs") {
		t.Fatalf("sidecar downloaded %v, want the image too", got)
	}
	if !slices.Equal(agent.sources, []string{"source.erofs"}) {
		t.Fatalf("mounted %v, want the downloaded image", agent.sources)
	}
}

// A download anything but its one mount reads stays the sidecar's: the file
// has to be in the workspace.
func TestRunner_KeepsDownloadsOtherArtifactsRead(t *testing.T) {
	srv, downloaded := objectServer(t)
	agent := &fakeFetcher{}
	arts := append(edgeArtifacts(srv.URL+"/a.erofs", srv.URL+"/env"),
		&artifact.Mount{ID: "again", In: "source.erofs", Out: "again"})

	runCombined(t, t.TempDir(), agent, arts)

	if len(agent.fetched) != 0 {
		t.Fatalf("fetched %v through the cache", agent.fetched)
	}
	if got := downloaded(); !slices.Contains(got, "/a.erofs") {
		t.Fatalf("sidecar downloaded %v, want the image", got)
	}
}
