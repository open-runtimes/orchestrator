package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"orchestrator/internal/artifact"
	"orchestrator/internal/sidecar"
	"orchestrator/internal/workload"
	"strconv"
	"sync"
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
	cfg  Config
	pool *pool                   // pool mode (cfg.ClaimToken set); nil = direct mode
	bind atomic.Pointer[binding] // armed at New in direct mode, by a claim in pool mode

	sem      chan struct{} // concurrency slots; nil = unlimited
	waiters  atomic.Int64  // requests queued for a slot
	inFlight atomic.Int64  // requests currently being proxied
	requests atomic.Int64  // cumulative accepted requests — the scrape-based idle signal
	draining atomic.Bool
	// closing is set once teardown begins and never cleared. It keeps a claim
	// from being accepted into a pod that is going away — which would hand its
	// caller a workload about to vanish, and could leave a mount behind that
	// release had already looked for.
	closing atomic.Bool
	// mountsReady gates the workload: the kubelet holds it until this sidecar's
	// startup probe passes, which it does once the mounts are established.
	mountsReady atomic.Bool

	// integral accumulates concurrency-seconds (∫ inFlight dt) as a monotonic
	// counter: instantaneous in-flight sampling under-reads fast handlers, so
	// the autoscaler derives true average concurrency from Δintegral/Δt
	// between its scrapes (the queue-proxy lesson).
	integralMu sync.Mutex
	integral   float64
	integralAt time.Time

	// mounts holds the claim's runner when it established image mounts, so
	// shutdown can release them. Nil when nothing was mounted.
	mounts atomic.Pointer[sidecar.Runner]

	deregisterDelay time.Duration // drainDeregisterDelay; shortened in tests
	// mounter performs the claim's image mounts. Nil uses the real one; unit
	// tests inject a fake, since mounting needs a privileged pod.
	mounter sidecar.Mounter

	runCtx context.Context // Start's ctx; late-armed probers run under it

	data    *http.Server
	admin   *http.Server
	dataLn  net.Listener
	adminLn net.Listener
}

// binding is the armed data plane: where to proxy, how to probe it, and how
// long a request may take. Direct mode arms it at New from cfg.Target; pool
// mode starts with none and arms it when an HTTP claim late-binds the target.
type binding struct {
	reverse *httputil.ReverseProxy
	prober  *prober
	timeout time.Duration // per-request total → 504

	// extra holds the claim's secondary ports, keyed by port. A request naming
	// one (workload.HeaderPort) is dialed there on loopback instead of Target; anything
	// not in here is refused, so the header can never widen what the claim
	// declared.
	extra map[int]string
}

func newBinding(cfg Config) *binding {
	var extra map[int]string
	if len(cfg.ExtraPorts) > 0 {
		extra = make(map[int]string, len(cfg.ExtraPorts))
		for _, port := range cfg.ExtraPorts {
			extra[port] = net.JoinHostPort(cfg.TargetHost, strconv.Itoa(port))
		}
	}
	return &binding{
		extra: extra,
		reverse: &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				// handleData resolved (and validated) which port this request is
				// for; the hint itself is ours, not the workload's, so it is
				// stripped before forwarding.
				target := cfg.Target
				if addr, ok := r.In.Context().Value(targetKey{}).(string); ok && addr != "" {
					target = addr
				}
				r.Out.Header.Del(workload.HeaderPort)
				r.SetURL(&url.URL{Scheme: "http", Host: target})
				r.SetXForwarded()
			},
			ErrorHandler: writeProxyError,
		},
		prober:  newProber(cfg),
		timeout: cfg.Timeout,
	}
}

// targetKey carries the resolved upstream address from handleData (which
// validates the port) into the proxy's Rewrite hook.
type targetKey struct{}

// target resolves which upstream address a request is for: the claim's primary
// port by default, a declared secondary port when workload.HeaderPort names one. An
// undeclared port is ("", false) — a 404, never a dial.
func (b *binding) target(r *http.Request, primary string) (string, bool) {
	raw := r.Header.Get(workload.HeaderPort)
	if raw == "" {
		return primary, true
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return "", false
	}
	if addr, ok := b.extra[port]; ok {
		return addr, true
	}
	return "", false
}

// New creates a proxy from cfg. Call Run (or Start) to serve.
func New(cfg Config) *Proxy {
	p := &Proxy{
		cfg:             cfg,
		deregisterDelay: drainDeregisterDelay,
	}
	if cfg.Concurrency > 0 {
		p.sem = make(chan struct{}, cfg.Concurrency)
	}
	if cfg.ClaimToken != "" {
		p.pool = newPool(cfg)
	} else {
		p.bind.Store(newBinding(cfg))
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
	p.awaitShutdown(ctx)
	return nil
}

// awaitShutdown blocks until ctx is cancelled, then drains and releases. Split
// from Run so a caller that has already Started — a test that needs the bound
// addresses before shutting down — does not have to race the bind.
func (p *Proxy) awaitShutdown(ctx context.Context) {
	<-ctx.Done()
	p.drain()
	p.release()
}

// release unmounts whatever the claim mounted, after in-flight requests have
// finished with it. The workspace propagates bidirectionally, so a mount left
// behind survives the pod and leaks on the node — this is not optional.
func (p *Proxy) release() {
	if r := p.mounts.Load(); r != nil {
		r.Release()
	}
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
	p.runCtx = ctx

	if b := p.bind.Load(); b != nil {
		go b.prober.run(ctx)
	}
	go func() { _ = p.data.Serve(dataLn) }()
	go func() { _ = p.admin.Serve(adminLn) }()

	// Mount before reporting mounts-ready, and only then does the kubelet start
	// the workload — the admin listener is already answering, so the startup
	// probe sees 503 until this returns. Pool mode mounts on the claim instead,
	// which is its own barrier.
	return p.mount(ctx)
}

// newRunner builds the artifact runner for a phase of this workload's life. The
// S3 credentials are the sidecar's, never the workload's — which is the reason a
// snapshot upload belongs here rather than in a command the workload runs.
func (p *Proxy) newRunner(id string) *sidecar.Runner {
	timeoutSeconds := int(p.cfg.Timeout / time.Second)
	if timeoutSeconds <= 0 {
		timeoutSeconds = int(p.cfg.MaxDrain / time.Second)
	}
	opts := []sidecar.Option{sidecar.WithS3Credentials(p.cfg.S3)}
	if p.mounter != nil {
		opts = append(opts, sidecar.WithMounter(p.mounter))
	}
	return sidecar.NewRunner(id, p.cfg.Workspace, timeoutSeconds, artifact.DefaultRegistry(), opts...)
}

// mount establishes a direct-mode workload's image mounts. Failing here fails
// Start: the workload must not run without them, and the pod's restart is the
// retry.
func (p *Proxy) mount(ctx context.Context) error {
	if p.cfg.ArtifactsJSON == "" {
		p.mountsReady.Store(true)
		return nil
	}
	artifacts, err := artifact.DefaultRegistry().Unmarshal([]byte(p.cfg.ArtifactsJSON))
	if err != nil {
		return fmt.Errorf("decode artifacts: %w", err)
	}
	if !p.cfg.Mounts {
		p.mountsReady.Store(true)
		return nil
	}
	runner := p.newRunner("direct")
	if err := runner.Mount(ctx, artifacts); err != nil {
		return err
	}
	p.mounts.Store(runner)
	p.mountsReady.Store(true)
	return nil
}

// Ready reports whether the fronted workload is passing its readiness probe.
// In pool mode with no target armed yet (unclaimed, or claimed for exec where
// there is nothing to probe) it reports warm-readiness instead: true unless
// the pod is poisoned. See handleReady for the full inversion story.
func (p *Proxy) Ready() bool {
	if b := p.bind.Load(); b != nil {
		return b.prober.Ready()
	}
	return p.pool != nil && !p.pool.snapshot().Failed
}

// DataAddr returns the data listener's bound address. Valid after Start.
func (p *Proxy) DataAddr() string { return p.dataLn.Addr().String() }

// AdminAddr returns the admin listener's bound address. Valid after Start.
func (p *Proxy) AdminAddr() string { return p.adminLn.Addr().String() }

func (p *Proxy) handleData(w http.ResponseWriter, r *http.Request) {
	b := p.bind.Load()
	if b == nil || p.draining.Load() || !b.prober.Ready() {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	release, ok := p.acquire(r.Context())
	if !ok {
		http.Error(w, "concurrency limit exceeded", http.StatusServiceUnavailable)
		return
	}
	defer release()

	p.accumulate(1)
	p.requests.Add(1)
	defer p.accumulate(-1)

	target, ok := b.target(r, p.cfg.Target)
	if !ok {
		http.Error(w, "port not exposed by this workload", http.StatusNotFound)
		return
	}

	ctx := context.WithValue(r.Context(), targetKey{}, target)
	if b.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}
	b.reverse.ServeHTTP(w, r.WithContext(ctx))
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
	mux.HandleFunc("GET "+workload.MountsReadyPath, p.handleMountsReady)
	if p.pool != nil {
		mux.HandleFunc("POST "+workload.ClaimPath, p.handleActivate)
		mux.HandleFunc("GET "+workload.ClaimStatePath, p.handleClaimState)
	}
	return mux
}

// handleMountsReady is the workload's start gate: until the mounts this sidecar
// owns are established, the kubelet must not start the container that reads
// them. A pod with nothing to mount answers 200 immediately.
func (p *Proxy) handleMountsReady(w http.ResponseWriter, _ *http.Request) {
	if !p.mountsReady.Load() {
		http.Error(w, "mounts not established", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleReady is the combined readiness signal probed by kubelet, Docker
// healthchecks, and the activator: container ready and not draining.
//
// Pool mode INVERTS what 200 means while unclaimed: it is warm-readiness —
// "this pod can accept an activation" (the replenishment controller's gate) —
// while the data plane is still 503 because nothing serves yet. An HTTP claim
// arms the target and /ready reverts to serving-readiness (the workload
// answers its probe); an exec claim keeps 200 with nothing to probe — the
// pod's fate is the container exit. A poisoned pod reports 503 forever.
func (p *Proxy) handleReady(w http.ResponseWriter, _ *http.Request) {
	if p.draining.Load() || !p.Ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// accumulate advances the concurrency-seconds integral and applies the
// in-flight delta, time-weighting the level held since the last transition.
func (p *Proxy) accumulate(delta int64) {
	now := time.Now()
	p.integralMu.Lock()
	if !p.integralAt.IsZero() {
		p.integral += float64(p.inFlight.Load()) * now.Sub(p.integralAt).Seconds()
	}
	p.integralAt = now
	p.integralMu.Unlock()
	p.inFlight.Add(delta)
}

// concurrencySeconds flushes the integral up to now and returns it.
func (p *Proxy) concurrencySeconds() float64 {
	now := time.Now()
	p.integralMu.Lock()
	defer p.integralMu.Unlock()
	if !p.integralAt.IsZero() {
		p.integral += float64(p.inFlight.Load()) * now.Sub(p.integralAt).Seconds()
		p.integralAt = now
	}
	return p.integral
}

// stats is the per-pod scrape payload for the autoscaler.
type stats struct {
	InFlight           int64   `json:"inFlight"`
	Requests           int64   `json:"requests"`           // cumulative; deltas of zero across a window mean idle
	ConcurrencySeconds float64 `json:"concurrencySeconds"` // monotonic ∫ inFlight dt; rate() it for average concurrency
	Ready              bool    `json:"ready"`
}

// requestBound is how long a request may take, as bound right now: the claim's
// value once one has armed the data plane, the configured default until then.
// 0 means unbounded. This is the only source for that fact — a claim that
// states its own bound (including 0) must not find a second answer waiting
// somewhere else.
func (p *Proxy) requestBound() time.Duration {
	if b := p.bind.Load(); b != nil {
		return b.timeout
	}
	return p.cfg.Timeout
}

// drainWindow is how long drain lets in-flight requests finish: as long as the
// bound in force allows them to take, capped by MaxDrain. An unbounded bound
// must not read as a zero window, so the cap is then MaxDrain alone.
func (p *Proxy) drainWindow() time.Duration {
	if bound := p.requestBound(); bound > 0 {
		return min(bound, p.cfg.MaxDrain)
	}
	return p.cfg.MaxDrain
}

func (p *Proxy) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats{
		InFlight:           p.inFlight.Load(),
		Requests:           p.requests.Load(),
		ConcurrencySeconds: p.concurrencySeconds(),
		Ready:              p.Ready(),
	})
}

// drain fails readiness so routing de-registers the pod, waits out
// propagation, then lets in-flight requests finish bounded by
// min(requestBound, MaxDrain) — requests still running at the deadline are
// dropped.
func (p *Proxy) drain() {
	p.closing.Store(true)
	p.draining.Store(true)
	time.Sleep(p.deregisterDelay)

	ctx, cancel := context.WithTimeout(context.Background(), p.drainWindow())
	defer cancel()
	_ = p.data.Shutdown(ctx)
	_ = p.admin.Close()
}
