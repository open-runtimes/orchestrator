package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"orchestrator/internal/sidecar"
	"orchestrator/internal/workload"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testClaimToken = "test-claim-token"

func poolConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig("")
	cfg.ClaimToken = testClaimToken
	cfg.TargetHost = "127.0.0.1"
	cfg.Workspace = t.TempDir()
	return cfg
}

// shimReader mimics the pool-shim: it creates the workspace FIFO and blocks
// reading one workload.ShimExec, delivered on the returned channel.
func shimReader(t *testing.T, workspace string) <-chan workload.ShimExec {
	t.Helper()
	path := filepath.Join(workspace, workload.ShimFIFOName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	ch := make(chan workload.ShimExec, 1)
	go func() {
		fifo, err := os.Open(path) // blocks until the sidecar opens the write end
		if err != nil {
			return
		}
		defer fifo.Close()
		var payload workload.ShimExec
		if err := json.NewDecoder(fifo).Decode(&payload); err != nil {
			return
		}
		ch <- payload
	}()
	return ch
}

// postActivate POSTs a claim with the given bearer token and returns the
// status code (0 on transport error, so it is goroutine-safe).
func postActivate(t *testing.T, p *Proxy, token string, claim workload.ClaimRequest) int {
	t.Helper()
	body, err := json.Marshal(claim)
	if err != nil {
		t.Errorf("marshal claim: %v", err)
		return 0
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, localURL(t, p.AdminAddr(), workload.ClaimPath), bytes.NewReader(body))
	if err != nil {
		t.Errorf("build claim request: %v", err)
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func claimState(t *testing.T, p *Proxy) workload.ClaimState {
	t.Helper()
	status, body := get(t, localURL(t, p.AdminAddr(), workload.ClaimStatePath))
	if status != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200", workload.ClaimStatePath, status)
	}
	var s workload.ClaimState
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("decode claim state %q: %v", body, err)
	}
	return s
}

func TestPoolStartsUnclaimed(t *testing.T) {
	p := startProxy(t, poolConfig(t))

	// Warm-ready inversion: /ready is 200 while unclaimed — the pod can
	// accept an activation — even though nothing serves yet.
	if status, _ := get(t, localURL(t, p.AdminAddr(), "/ready")); status != http.StatusOK {
		t.Fatalf("/ready unclaimed: got %d, want 200", status)
	}
	if status, _ := get(t, localURL(t, p.DataAddr(), "/")); status != http.StatusServiceUnavailable {
		t.Fatalf("data unclaimed: got %d, want 503", status)
	}
	if s := claimState(t, p); s.Claimed || s.Failed {
		t.Fatalf("unclaimed state: got %+v, want zero", s)
	}
}

func TestPoolClaimSignalsShim(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	}))
	t.Cleanup(backend.Close)
	_, portStr, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split backend addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	cfg := poolConfig(t)
	execCh := shimReader(t, cfg.Workspace)
	p := startProxy(t, cfg)

	status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-1",
		Command:      "node index.js",
		Environment:  map[string]string{"FOO": "bar"},
		Port:         port,
	})
	if status != http.StatusOK {
		t.Fatalf("claim: got %d, want 200", status)
	}

	select {
	case payload := <-execCh:
		if payload.Command != "node index.js" || payload.Environment["FOO"] != "bar" || payload.WorkDir != cfg.Workspace {
			t.Fatalf("shim payload: got %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shim never received the exec payload")
	}

	if s := claimState(t, p); !s.Claimed || s.Failed || s.ActivationID != "act-1" {
		t.Fatalf("claimed state: got %+v, want claimed act-1", s)
	}
	if status := postActivate(t, p, testClaimToken, workload.ClaimRequest{ActivationID: "act-2", Command: "true", Port: port}); status != http.StatusConflict {
		t.Fatalf("second claim: got %d, want 409", status)
	}
}

func TestPoolClaimRace(t *testing.T) {
	cfg := poolConfig(t)
	shimReader(t, cfg.Workspace) // the single winner still signals the shim
	p := startProxy(t, cfg)

	const racers = 20
	statuses := make(chan int, racers)
	start := make(chan struct{})
	for i := range racers {
		go func() {
			<-start
			statuses <- postActivate(t, p, testClaimToken, workload.ClaimRequest{
				ActivationID: "act-" + strconv.Itoa(i),
				Command:      "true",
			})
		}()
	}
	close(start)

	var won, conflicted int
	for range racers {
		switch <-statuses {
		case http.StatusOK:
			won++
		case http.StatusConflict:
			conflicted++
		}
	}
	if won != 1 || conflicted != racers-1 {
		t.Fatalf("race: got %d wins and %d conflicts, want 1 and %d", won, conflicted, racers-1)
	}
}

func TestPoolClaimBadToken(t *testing.T) {
	p := startProxy(t, poolConfig(t))

	if status := postActivate(t, p, "wrong-token", workload.ClaimRequest{ActivationID: "act-1", Command: "true"}); status != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d, want 401", status)
	}
	if s := claimState(t, p); s.Claimed {
		t.Fatalf("state after bad token: got %+v, want unclaimed", s)
	}
	if status, _ := get(t, localURL(t, p.AdminAddr(), "/ready")); status != http.StatusOK {
		t.Fatalf("/ready after bad token: got %d, want 200 (still warm)", status)
	}
}

func TestPoolClaimHTTP(t *testing.T) {
	var healthy atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/block", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // holds until the proxy's per-request timeout fires
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	})
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)
	_, portStr, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split backend addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	cfg := poolConfig(t)
	cfg.ReadinessPath = "/healthz"
	execCh := shimReader(t, cfg.Workspace)
	p := startProxy(t, cfg)

	status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID:   "act-http",
		Command:        "serve",
		Port:           port,
		TimeoutSeconds: ptrTo(1),
	})
	if status != http.StatusOK {
		t.Fatalf("HTTP claim: got %d, want 200", status)
	}
	<-execCh

	// The claim armed the prober: /ready now means serving-readiness, and
	// the workload is not healthy yet.
	if status, _ := get(t, localURL(t, p.AdminAddr(), "/ready")); status != http.StatusServiceUnavailable {
		t.Fatalf("/ready after claim, workload unhealthy: got %d, want 503", status)
	}

	healthy.Store(true)
	waitReady(t, p)
	status, body := get(t, localURL(t, p.DataAddr(), "/"))
	if status != http.StatusOK || body != "hello" {
		t.Fatalf("proxied request: got %d %q, want 200 %q", status, body, "hello")
	}

	// The claim's TimeoutSeconds is the per-request timeout.
	if status, _ := get(t, localURL(t, p.DataAddr(), "/block")); status != http.StatusGatewayTimeout {
		t.Fatalf("blocked request: got %d, want 504", status)
	}
}

func TestPoolClaimArtifactFailurePoisons(t *testing.T) {
	p := startProxy(t, poolConfig(t)) // no FIFO: artifacts fail before signaling

	status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-bad",
		Command:      "true",
		Artifacts:    []artifact.Artifact{&artifact.Read{ID: "missing", In: "does-not-exist.json"}},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("failing claim: got %d, want 422", status)
	}

	s := claimState(t, p)
	if !s.Claimed || !s.Failed || s.Error == "" {
		t.Fatalf("poisoned state: got %+v, want claimed+failed with error", s)
	}
	if status, _ := get(t, localURL(t, p.AdminAddr(), "/ready")); status != http.StatusServiceUnavailable {
		t.Fatalf("/ready poisoned: got %d, want 503", status)
	}
	if status := postActivate(t, p, testClaimToken, workload.ClaimRequest{ActivationID: "act-retry", Command: "true"}); status != http.StatusConflict {
		t.Fatalf("claim after poison: got %d, want 409", status)
	}
}

// fakeMounter stands in for the kernel: mounting needs a privileged pod, which a
// unit test does not have. It records what was mounted and released, in order.
type fakeMounter struct {
	mu       sync.Mutex
	mounted  []string
	released []string
	err      error
}

func (f *fakeMounter) Mount(_, target string, _ sidecar.MountOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.mounted = append(f.mounted, target)
	return nil
}

func (f *fakeMounter) Unmount(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, target)
	return nil
}

// writeWorkspaceFile stands in for the artifact that would have produced the
// image a mount consumes.
func writeWorkspaceFile(t *testing.T, workspace, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, name), []byte("image"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func (f *fakeMounter) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mounted), len(f.released)
}

// A mount needs a privileged sidecar and a propagating workspace, both fixed
// when the warm pod was created. A pool without the capability must refuse the
// claim rather than start a workload whose mount is silently absent.
func TestPoolClaim_RefusesAMountThePoolCannotPerform(t *testing.T) {
	p := startProxy(t, poolConfig(t)) // Mounts defaults to false

	status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-mount",
		Command:      "true",
		Artifacts:    []artifact.Artifact{&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"}},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("claim with a mount: got %d, want 422", status)
	}
	s := claimState(t, p)
	if !s.Failed || !strings.Contains(s.Error, "mounts on the pool") {
		t.Fatalf("state should name the pool setting, got %+v", s)
	}
}

// With the capability, the mount is established BEFORE the shim is signalled —
// so the payload finds it in place rather than racing it.
func TestPoolClaim_MountsBeforeSignallingTheWorkload(t *testing.T) {
	cfg := poolConfig(t)
	cfg.Mounts = true
	mounter := &fakeMounter{}
	execCh := shimReader(t, cfg.Workspace)
	// The image is normally produced by a preceding artifact; the mount waits
	// for it either way.
	writeWorkspaceFile(t, cfg.Workspace, "data.sqfs")
	p := startProxy(t, cfg)
	p.mounter = mounter

	status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-mount",
		Command:      "serve",
		Port:         backendOnLoopback(t, "ok"),
		Artifacts:    []artifact.Artifact{&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"}},
	})
	if status != http.StatusOK {
		t.Fatalf("claim: got %d, want 200 (%+v)", status, claimState(t, p))
	}

	// Receiving the exec line proves the claim got past Mount, so a mount
	// recorded by now was established first.
	<-execCh
	if mounted, _ := mounter.counts(); mounted != 1 {
		t.Fatalf("want the image mounted before the workload was signalled, got %d mounts", mounted)
	}
}

// A failed mount poisons the pod: the workload never starts, and the claim says
// why.
func TestPoolClaim_FailedMountPoisons(t *testing.T) {
	cfg := poolConfig(t)
	cfg.Mounts = true
	writeWorkspaceFile(t, cfg.Workspace, "data.sqfs")
	p := startProxy(t, cfg)
	p.mounter = &fakeMounter{err: errors.New("not a filesystem image")}

	status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-bad-mount",
		Command:      "serve",
		Artifacts:    []artifact.Artifact{&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"}},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("failed mount: got %d, want 422", status)
	}
	if s := claimState(t, p); !s.Failed || !strings.Contains(s.Error, "not a filesystem image") {
		t.Fatalf("state should carry the reason, got %+v", s)
	}
}

// The workspace propagates bidirectionally, so a mount left behind outlives the
// pod on its node. Shutdown must release it — after the drain, so in-flight
// requests are not reading from a filesystem pulled out from under them.
func TestPoolClaim_ReleasesMountsOnShutdown(t *testing.T) {
	cfg := poolConfig(t)
	cfg.Mounts = true
	cfg.Target = ""
	mounter := &fakeMounter{}
	execCh := shimReader(t, cfg.Workspace)
	writeWorkspaceFile(t, cfg.Workspace, "data.sqfs")

	ctx, cancel := context.WithCancel(t.Context())
	p := New(cfg)
	p.deregisterDelay = time.Millisecond
	p.mounter = mounter
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	done := make(chan struct{})
	go func() { p.awaitShutdown(ctx); close(done) }()

	if status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-mount",
		Command:      "serve",
		Port:         backendOnLoopback(t, "ok"),
		Artifacts:    []artifact.Artifact{&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"}},
	}); status != http.StatusOK {
		t.Fatalf("claim: got %d", status)
	}
	<-execCh
	if mounted, released := mounter.counts(); mounted != 1 || released != 0 {
		t.Fatalf("before shutdown: %d mounted, %d released", mounted, released)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return")
	}
	if mounted, released := mounter.counts(); released != mounted {
		t.Errorf("shutdown left mounts behind: %d mounted, %d released", mounted, released)
	}
}

// The drain window follows the claim's bound in practice, not just in the rule.
// Shutdown does not cut an in-flight request when the window expires — it stops
// waiting for it, and the process exits from under it. So the observable
// promise is that drain keeps waiting for a request the claim still allows.
// This is the sandbox case: claimed for long sessions on a pod whose configured
// default is short.
func TestPoolClaim_DrainWaitsAsLongAsTheClaimAllows(t *testing.T) {
	backend, release := blockingBackend(t)
	cfg := poolConfig(t)
	cfg.Timeout = 50 * time.Millisecond // the static default, overridden by the claim
	cfg.MaxDrain = 5 * time.Second
	execCh := shimReader(t, cfg.Workspace)
	p := startProxy(t, cfg)

	if status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID:   "act-session",
		Command:        "serve",
		Port:           backendPort(t, backend),
		TimeoutSeconds: ptrTo(2),
	}); status != http.StatusOK {
		t.Fatalf("claim: got %d, want 200", status)
	}
	<-execCh
	waitReady(t, p)

	statuses := make(chan int, 1)
	asyncGet(t, localURL(t, p.DataAddr(), "/"), statuses)
	waitFor(t, "request in flight", func() bool { return p.inFlight.Load() == 1 })

	drained := make(chan struct{})
	go func() {
		p.drain()
		close(drained)
	}()

	// Well past the 50ms static window that used to bound the drain, and well
	// inside the 2s the claim asked for: drain must still be waiting.
	time.Sleep(400 * time.Millisecond)
	select {
	case <-drained:
		t.Fatal("drain stopped waiting for a request the claim still allows")
	default:
	}

	close(release)
	if status := <-statuses; status != http.StatusOK {
		t.Fatalf("in-flight request during drain: got %d, want 200 — cut by a window the claim overrode", status)
	}
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish")
	}
}

// The claim decides how long a request may take, and that one answer decides the
// drain window too — an unbounded claim drains for MaxDrain rather than for no
// time at all.
func TestPoolClaim_TimeoutSeconds(t *testing.T) {
	for name, tc := range map[string]struct {
		claim      *int
		want       time.Duration
		wantWindow time.Duration
	}{
		// MaxDrain is 60s below, so it caps the window without hiding the bound.
		"stated":    {claim: ptrTo(30), want: 30 * time.Second, wantWindow: 30 * time.Second},
		"unbounded": {claim: ptrTo(0), want: 0, wantWindow: 60 * time.Second},
		"unstated":  {claim: nil, want: 300 * time.Second, wantWindow: 60 * time.Second}, // the pod's own default
	} {
		t.Run(name, func(t *testing.T) {
			cfg := poolConfig(t)
			cfg.Timeout = 300 * time.Second
			cfg.MaxDrain = 60 * time.Second
			execCh := shimReader(t, cfg.Workspace)
			p := startProxy(t, cfg)

			if status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
				ActivationID:   "act-" + name,
				Command:        "serve",
				Port:           backendOnLoopback(t, "ok"),
				TimeoutSeconds: tc.claim,
			}); status != http.StatusOK {
				t.Fatalf("claim: got %d, want 200", status)
			}
			<-execCh

			if got := p.requestBound(); got != tc.want {
				t.Errorf("per-request bound: want %v, got %v", tc.want, got)
			}
			if got := p.drainWindow(); got != tc.wantWindow {
				t.Errorf("drain window: want %v, got %v", tc.wantWindow, got)
			}
		})
	}
}

// A claim may declare extra ports. They are addressable through the same data
// listener via workload.HeaderPort — dialed on loopback inside this pod, so the header
// can never reach another pod, and never a port the claim did not declare.
func TestPoolClaim_ExtraPorts(t *testing.T) {
	primary := backendOnLoopback(t, "primary")
	extra := backendOnLoopback(t, "extra")

	cfg := poolConfig(t)
	execCh := shimReader(t, cfg.Workspace)
	p := startProxy(t, cfg)

	if status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-ports",
		Command:      "serve",
		Port:         primary,
		Ports:        []int{extra},
	}); status != http.StatusOK {
		t.Fatalf("claim: got %d, want 200", status)
	}
	<-execCh

	waitReady(t, p)

	// No hint → the primary port, exactly as before extra ports existed.
	if _, body := get(t, localURL(t, p.DataAddr(), "/")); body != "primary" {
		t.Errorf("unhinted request: got %q, want primary", body)
	}
	// The declared extra.
	if status, body := getWithPort(t, p, strconv.Itoa(extra)); status != http.StatusOK || body != "extra" {
		t.Errorf("declared port: got %d %q, want 200 extra", status, body)
	}
	// An undeclared port is refused rather than dialed — including the
	// sidecar's own admin port.
	for _, port := range []string{"1", strconv.Itoa(workload.DefaultProxyPort), strconv.Itoa(workload.DefaultAdminPort), "notaport"} {
		if status, _ := getWithPort(t, p, port); status != http.StatusNotFound {
			t.Errorf("undeclared port %s: got %d, want 404", port, status)
		}
	}
}

// The port hint is the machinery's, not the workload's: it must not reach the
// upstream.
func TestPoolClaim_PortHintStrippedFromUpstream(t *testing.T) {
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(workload.HeaderPort)
	}))
	t.Cleanup(backend.Close)
	port := backendPort(t, backend)

	cfg := poolConfig(t)
	execCh := shimReader(t, cfg.Workspace)
	p := startProxy(t, cfg)
	if status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-strip", Command: "serve", Port: port, Ports: []int{port},
	}); status != http.StatusOK {
		t.Fatalf("claim: got %d, want 200", status)
	}
	<-execCh
	waitReady(t, p)

	if status, _ := getWithPort(t, p, strconv.Itoa(port)); status != http.StatusOK {
		t.Fatalf("request: got %d, want 200", status)
	}
	if hint := <-seen; hint != "" {
		t.Errorf("workload saw the port hint: %q", hint)
	}
}

// backendOnLoopback starts a server answering with body and returns its port.
// backendOnLoopback starts a server answering with body and returns its port.
func backendOnLoopback(t *testing.T, body string) int {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(backend.Close)
	return backendPort(t, backend)
}

func backendPort(t *testing.T, backend *httptest.Server) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split backend addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	return port
}

// getWithPort issues a data-plane request naming a port, the way the sandbox
// edge does.
func getWithPort(t *testing.T, p *Proxy, port string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, localURL(t, p.DataAddr(), "/"), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(workload.HeaderPort, port)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func ptrTo[T any](v T) *T { return &v }

// A pod being torn down must not accept a claim. Accepting one would hand its
// caller a workload about to vanish, and a mount established after release has
// already looked would be left on the node — bidirectional propagation means it
// outlives the pod.
func TestPoolClaim_RefusedOnceShuttingDown(t *testing.T) {
	cfg := poolConfig(t)
	cfg.Mounts = true
	mounter := &fakeMounter{}
	writeWorkspaceFile(t, cfg.Workspace, "data.sqfs")
	p := startProxy(t, cfg)
	p.mounter = mounter

	// The window that matters is the drain itself: readiness is failing and the
	// admin listener is still up, which is exactly when a backend's in-flight
	// claim arrives. (Once drain finishes the listener is closed and the claim
	// cannot reach the pod at all.)
	p.deregisterDelay = 750 * time.Millisecond
	go p.drain()
	waitFor(t, "teardown started", p.closing.Load)

	status := postActivate(t, p, testClaimToken, workload.ClaimRequest{
		ActivationID: "act-late",
		Command:      "serve",
		Artifacts:    []artifact.Artifact{&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"}},
	})
	if status != http.StatusConflict {
		t.Fatalf("a claim during teardown: got %d, want 409", status)
	}
	if mounted, _ := mounter.counts(); mounted != 0 {
		t.Errorf("nothing should have been mounted, got %d", mounted)
	}
}
