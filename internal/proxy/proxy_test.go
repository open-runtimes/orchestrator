package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(target string) Config {
	return Config{
		Target:                    target,
		Timeout:                   time.Second,
		MaxDrain:                  time.Second,
		QueueSize:                 100,
		ReadinessPeriod:           5 * time.Millisecond,
		ReadinessTimeout:          100 * time.Millisecond,
		ReadinessFailureThreshold: 3,
	}
}

// startProxy starts cfg on ephemeral ports with a fast drain, stopping the
// servers when the test ends.
func startProxy(t *testing.T, cfg Config) *Proxy {
	t.Helper()
	p := New(cfg)
	p.deregisterDelay = time.Millisecond
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = p.data.Close()
		_ = p.admin.Close()
	})
	return p
}

// localURL rewrites a bound wildcard address into a dialable loopback URL.
func localURL(t *testing.T, boundAddr, path string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		t.Fatalf("split %q: %v", boundAddr, err)
	}
	return "http://127.0.0.1:" + port + path
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func waitReady(t *testing.T, p *Proxy) {
	t.Helper()
	waitFor(t, "proxy ready", p.Ready)
}

// blockingBackend serves requests that block until release is closed.
func blockingBackend(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			_, _ = io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv, release
}

// asyncGet runs a GET in a goroutine and reports the status code (0 on error).
func asyncGet(t *testing.T, url string, statuses chan<- int) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			statuses <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		statuses <- resp.StatusCode
	}()
}

func TestReadinessGating(t *testing.T) {
	var healthy atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	})
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)

	cfg := testConfig(backend.Listener.Addr().String())
	cfg.ReadinessPath = "/healthz"
	p := startProxy(t, cfg)

	if status, _ := get(t, localURL(t, p.DataAddr(), "/")); status != http.StatusServiceUnavailable {
		t.Fatalf("data before ready: got %d, want 503", status)
	}
	if status, _ := get(t, localURL(t, p.AdminAddr(), "/ready")); status != http.StatusServiceUnavailable {
		t.Fatalf("/ready before ready: got %d, want 503", status)
	}

	healthy.Store(true)
	waitReady(t, p)

	status, body := get(t, localURL(t, p.DataAddr(), "/"))
	if status != http.StatusOK || body != "hello" {
		t.Fatalf("data after ready: got %d %q, want 200 %q", status, body, "hello")
	}
	if status, _ := get(t, localURL(t, p.AdminAddr(), "/ready")); status != http.StatusOK {
		t.Fatalf("/ready after ready: got %d, want 200", status)
	}
}

func TestSlowBackendTimesOut(t *testing.T) {
	backend, _ := blockingBackend(t) // never released
	cfg := testConfig(backend.Listener.Addr().String())
	cfg.Timeout = 50 * time.Millisecond
	p := startProxy(t, cfg)
	waitReady(t, p)

	if status, _ := get(t, localURL(t, p.DataAddr(), "/")); status != http.StatusGatewayTimeout {
		t.Fatalf("slow backend: got %d, want 504", status)
	}
}

func TestConcurrencyCapShedsOverflow(t *testing.T) {
	backend, release := blockingBackend(t)
	cfg := testConfig(backend.Listener.Addr().String())
	cfg.Concurrency = 1
	cfg.QueueSize = 1
	p := startProxy(t, cfg)
	waitReady(t, p)
	url := localURL(t, p.DataAddr(), "/")

	statuses := make(chan int, 2)
	asyncGet(t, url, statuses) // fills the single slot
	waitFor(t, "first request in flight", func() bool { return p.inFlight.Load() == 1 })
	asyncGet(t, url, statuses) // queues
	waitFor(t, "second request queued", func() bool { return p.waiters.Load() == 1 })

	if status, _ := get(t, url); status != http.StatusServiceUnavailable {
		t.Fatalf("overflow request: got %d, want 503", status)
	}

	close(release)
	for range 2 {
		if status := <-statuses; status != http.StatusOK {
			t.Fatalf("capped request: got %d, want 200", status)
		}
	}
}

func TestDrainCompletesInFlight(t *testing.T) {
	backend, release := blockingBackend(t)
	p := startProxy(t, testConfig(backend.Listener.Addr().String()))
	p.deregisterDelay = 100 * time.Millisecond
	waitReady(t, p)

	statuses := make(chan int, 1)
	asyncGet(t, localURL(t, p.DataAddr(), "/"), statuses)
	waitFor(t, "request in flight", func() bool { return p.inFlight.Load() == 1 })

	readyURL := localURL(t, p.AdminAddr(), "/ready")
	drained := make(chan struct{})
	go func() {
		p.drain()
		close(drained)
	}()

	// During the de-register window readiness must fail while the in-flight
	// request keeps running.
	waitFor(t, "draining flag", p.draining.Load)
	if status, _ := get(t, readyURL); status != http.StatusServiceUnavailable {
		t.Fatalf("/ready while draining: got %d, want 503", status)
	}

	close(release)
	if status := <-statuses; status != http.StatusOK {
		t.Fatalf("in-flight request during drain: got %d, want 200", status)
	}
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not finish")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(backend.Close)
	p := New(testConfig(backend.Listener.Addr().String()))
	p.deregisterDelay = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestStats(t *testing.T) {
	backend, release := blockingBackend(t)
	p := startProxy(t, testConfig(backend.Listener.Addr().String()))
	waitReady(t, p)

	statsURL := localURL(t, p.AdminAddr(), "/stats")
	fetch := func() stats {
		_, body := get(t, statsURL)
		if !strings.Contains(body, `"inFlight"`) {
			t.Fatalf("stats body %q missing inFlight key", body)
		}
		var s stats
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("decode stats %q: %v", body, err)
		}
		return s
	}

	if s := fetch(); s.InFlight != 0 || !s.Ready {
		t.Fatalf("idle stats: got %+v, want inFlight 0 ready true", s)
	}

	statuses := make(chan int, 1)
	asyncGet(t, localURL(t, p.DataAddr(), "/"), statuses)
	waitFor(t, "stats to report in-flight request", func() bool { return fetch().InFlight == 1 })

	close(release)
	if status := <-statuses; status != http.StatusOK {
		t.Fatalf("blocked request: got %d, want 200", status)
	}
	waitFor(t, "stats to drop to zero", func() bool { return fetch().InFlight == 0 })
}
