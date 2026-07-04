package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// drainDeregisterDelay is how long the proxy keeps draining-503s flowing after
// failing readiness, so routing layers de-register the pod before in-flight
// draining begins.
const drainDeregisterDelay = 2 * time.Second

// Proxy fronts the user container: proxied traffic on ProxyPort, /ready and
// /stats on AdminPort.
type Proxy struct {
	cfg     Config
	prober  *prober
	reverse *httputil.ReverseProxy

	sem      chan struct{} // concurrency slots; nil = unlimited
	waiters  atomic.Int64  // requests queued for a slot
	inFlight atomic.Int64  // requests currently being proxied
	draining atomic.Bool

	deregisterDelay time.Duration // drainDeregisterDelay; shortened in tests

	data    *http.Server
	admin   *http.Server
	dataLn  net.Listener
	adminLn net.Listener
}

// New creates a proxy from cfg. Call Run (or Start) to serve.
func New(cfg Config) *Proxy {
	p := &Proxy{
		cfg:             cfg,
		prober:          newProber(cfg),
		deregisterDelay: drainDeregisterDelay,
	}
	if cfg.Concurrency > 0 {
		p.sem = make(chan struct{}, cfg.Concurrency)
	}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: cfg.Target})
			r.SetXForwarded()
		},
		ErrorHandler: writeProxyError,
	}
	p.data = &http.Server{Handler: http.HandlerFunc(p.handleData)}
	p.admin = &http.Server{Handler: p.adminMux()}
	return p
}

// Run serves until ctx is cancelled, then drains gracefully. It returns
// non-nil only if the listeners could not be bound.
func (p *Proxy) Run(ctx context.Context) error {
	if err := p.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	p.drain()
	return nil
}

// Start binds both listeners (port 0 = ephemeral) and serves in the
// background. The readiness prober stops when ctx is cancelled.
func (p *Proxy) Start(ctx context.Context) error {
	var lc net.ListenConfig
	dataLn, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(p.cfg.ProxyPort))
	if err != nil {
		return err
	}
	adminLn, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(p.cfg.AdminPort))
	if err != nil {
		_ = dataLn.Close()
		return err
	}
	p.dataLn, p.adminLn = dataLn, adminLn

	go p.prober.run(ctx)
	go func() { _ = p.data.Serve(dataLn) }()
	go func() { _ = p.admin.Serve(adminLn) }()
	return nil
}

// Ready reports whether the user container is passing its readiness probe.
func (p *Proxy) Ready() bool { return p.prober.Ready() }

// DataAddr returns the data listener's bound address. Valid after Start.
func (p *Proxy) DataAddr() string { return p.dataLn.Addr().String() }

// AdminAddr returns the admin listener's bound address. Valid after Start.
func (p *Proxy) AdminAddr() string { return p.adminLn.Addr().String() }

func (p *Proxy) handleData(w http.ResponseWriter, r *http.Request) {
	if p.draining.Load() || !p.prober.Ready() {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	release, ok := p.acquire(r.Context())
	if !ok {
		http.Error(w, "concurrency limit exceeded", http.StatusServiceUnavailable)
		return
	}
	defer release()

	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)

	ctx := r.Context()
	if p.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.Timeout)
		defer cancel()
	}
	p.reverse.ServeHTTP(w, r.WithContext(ctx))
}

// acquire reserves a concurrency slot. Requests beyond Concurrency wait in a
// queue bounded by QueueSize; beyond that they are shed immediately. Queued
// requests are released when the client gives up (ctx done).
func (p *Proxy) acquire(ctx context.Context) (func(), bool) {
	if p.sem == nil {
		return func() {}, true
	}
	select {
	case p.sem <- struct{}{}:
		return p.releaseSlot, true
	default:
	}
	if p.waiters.Add(1) > int64(p.cfg.QueueSize) {
		p.waiters.Add(-1)
		return nil, false
	}
	defer p.waiters.Add(-1)
	select {
	case p.sem <- struct{}{}:
		return p.releaseSlot, true
	case <-ctx.Done():
		return nil, false
	}
}

func (p *Proxy) releaseSlot() { <-p.sem }

// writeProxyError maps transport failures: request deadline → 504, everything
// else (dial and connection errors) → 502, with no upstream detail leaked.
func writeProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		return
	}
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

func (p *Proxy) adminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", p.handleReady)
	mux.HandleFunc("GET /stats", p.handleStats)
	return mux
}

// handleReady is the combined readiness signal probed by kubelet, Docker
// healthchecks, and the activator: container ready and not draining.
func (p *Proxy) handleReady(w http.ResponseWriter, _ *http.Request) {
	if p.draining.Load() || !p.prober.Ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// stats is the per-pod scrape payload for the autoscaler.
type stats struct {
	InFlight int64 `json:"inFlight"`
	Ready    bool  `json:"ready"`
}

func (p *Proxy) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats{
		InFlight: p.inFlight.Load(),
		Ready:    p.prober.Ready(),
	})
}

// drain fails readiness so routing de-registers the pod, waits out
// propagation, then lets in-flight requests finish bounded by
// min(Timeout, MaxDrain) — requests still running at the deadline are dropped.
func (p *Proxy) drain() {
	p.draining.Store(true)
	time.Sleep(p.deregisterDelay)

	ctx, cancel := context.WithTimeout(context.Background(), min(p.cfg.Timeout, p.cfg.MaxDrain))
	defer cancel()
	_ = p.data.Shutdown(ctx)
	_ = p.admin.Close()
}
