package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// reading one ShimExec, delivered on the returned channel.
func shimReader(t *testing.T, workspace string) <-chan ShimExec {
	t.Helper()
	path := filepath.Join(workspace, ShimFIFOName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	ch := make(chan ShimExec, 1)
	go func() {
		fifo, err := os.Open(path) // blocks until the sidecar opens the write end
		if err != nil {
			return
		}
		defer fifo.Close()
		var payload ShimExec
		if err := json.NewDecoder(fifo).Decode(&payload); err != nil {
			return
		}
		ch <- payload
	}()
	return ch
}

// postActivate POSTs a claim with the given bearer token and returns the
// status code (0 on transport error, so it is goroutine-safe).
func postActivate(t *testing.T, p *Proxy, token string, claim ClaimRequest) int {
	t.Helper()
	body, err := json.Marshal(claim)
	if err != nil {
		t.Errorf("marshal claim: %v", err)
		return 0
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, localURL(t, p.AdminAddr(), ClaimPath), bytes.NewReader(body))
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

func claimState(t *testing.T, p *Proxy) ClaimState {
	t.Helper()
	status, body := get(t, localURL(t, p.AdminAddr(), ClaimStatePath))
	if status != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200", ClaimStatePath, status)
	}
	var s ClaimState
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

	status := postActivate(t, p, testClaimToken, ClaimRequest{
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
	if status := postActivate(t, p, testClaimToken, ClaimRequest{ActivationID: "act-2", Command: "true", Port: port}); status != http.StatusConflict {
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
			statuses <- postActivate(t, p, testClaimToken, ClaimRequest{
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

	if status := postActivate(t, p, "wrong-token", ClaimRequest{ActivationID: "act-1", Command: "true"}); status != http.StatusUnauthorized {
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

	status := postActivate(t, p, testClaimToken, ClaimRequest{
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

	status := postActivate(t, p, testClaimToken, ClaimRequest{
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
	if status := postActivate(t, p, testClaimToken, ClaimRequest{ActivationID: "act-retry", Command: "true"}); status != http.StatusConflict {
		t.Fatalf("claim after poison: got %d, want 409", status)
	}
}

// A mount needs a post-phase sidecar to establish it and undo it, and this
// sidecar runs a pre phase only. The claim must be refused rather than
// materializing everything else and starting a workload whose mount is silently
// absent. (The API rejects these at validation; this is the boundary refusing
// them too.)
func TestPoolClaim_RefusesAMountItCannotEstablish(t *testing.T) {
	p := startProxy(t, poolConfig(t))

	status := postActivate(t, p, testClaimToken, ClaimRequest{
		ActivationID: "act-mount",
		Command:      "true",
		Artifacts:    []artifact.Artifact{&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"}},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("claim with a mount: got %d, want 422", status)
	}
	s := claimState(t, p)
	if !s.Failed || !strings.Contains(s.Error, "post-phase") {
		t.Fatalf("state should name what is missing, got %+v", s)
	}
}

// A claim may declare extra ports. They are addressable through the same data
// listener via HeaderPort — dialed on loopback inside this pod, so the header
// can never reach another pod, and never a port the claim did not declare.
func TestPoolClaim_ExtraPorts(t *testing.T) {
	primary := backendOnLoopback(t, "primary")
	extra := backendOnLoopback(t, "extra")

	cfg := poolConfig(t)
	execCh := shimReader(t, cfg.Workspace)
	p := startProxy(t, cfg)

	if status := postActivate(t, p, testClaimToken, ClaimRequest{
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
	for _, port := range []string{"1", strconv.Itoa(DefaultProxyPort), strconv.Itoa(DefaultAdminPort), "notaport"} {
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
		seen <- r.Header.Get(HeaderPort)
	}))
	t.Cleanup(backend.Close)
	port := backendPort(t, backend)

	cfg := poolConfig(t)
	execCh := shimReader(t, cfg.Workspace)
	p := startProxy(t, cfg)
	if status := postActivate(t, p, testClaimToken, ClaimRequest{
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
	req.Header.Set(HeaderPort, port)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func ptrTo[T any](v T) *T { return &v }

// A claim's timeout is installed as given — including a 0, which asks for no
// bound at all. Anything else cuts the long-lived sessions (WebSocket
// terminals, language servers) that request it, at whatever default the pod
// happened to be configured with.
func TestPoolClaim_TimeoutSeconds(t *testing.T) {
	for name, tc := range map[string]struct {
		claim *int
		want  time.Duration
	}{
		"stated":    {claim: ptrTo(30), want: 30 * time.Second},
		"unbounded": {claim: ptrTo(0), want: 0},
		"unstated":  {claim: nil, want: 300 * time.Second}, // the pod's own default
	} {
		t.Run(name, func(t *testing.T) {
			cfg := poolConfig(t)
			cfg.Timeout = 300 * time.Second
			execCh := shimReader(t, cfg.Workspace)
			p := startProxy(t, cfg)

			if status := postActivate(t, p, testClaimToken, ClaimRequest{
				ActivationID:   "act-" + name,
				Command:        "serve",
				Port:           backendOnLoopback(t, "ok"),
				TimeoutSeconds: tc.claim,
			}); status != http.StatusOK {
				t.Fatalf("claim: got %d, want 200", status)
			}
			<-execCh

			if got := p.bind.Load().timeout; got != tc.want {
				t.Errorf("per-request timeout: want %v, got %v", tc.want, got)
			}
		})
	}
}
