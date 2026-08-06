package activator

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

const (
	endpointPollInterval = 100 * time.Millisecond

	// raiseDebounce bounds how often a cold workload's scale-up is
	// re-requested while requests wait for the first endpoint.
	raiseDebounce = 2 * time.Second
)

// Components that run a broker. The values match each one's
// app.kubernetes.io/component label, so a PromQL series and a pod selector read
// the same.
const (
	componentActivator    = "deployments-activator"
	componentSandboxProxy = "sandbox-proxy"
)

// capacity is what the caller knows about reaching one workload: how to find a
// serving endpoint. Bound per request (to a deployment spec on Docker, a
// revision on Kubernetes, a sandbox token); the broker owns everything either
// side of it.
type capacity interface {
	// Target returns a reachable endpoint, or nil when none is ready yet.
	Target(ctx context.Context) (*url.URL, error)
}

// riser is the capacity of a workload that can be made to appear: a deployment
// scaled to zero rises on demand. A capacity that does not implement it has
// nothing to raise — a sandbox is a claimed workload, so if it is gone, it is
// gone — and the broker then only waits, without reporting raises it never
// performed.
type riser interface {
	// Raise requests capacity for a cold workload. The broker debounces calls
	// per key; implementations own idempotence and success logging.
	Raise(ctx context.Context) error
}

// Recorder receives the activator's domain metrics. Satisfied by
// *observability.Metrics; nil disables recording.
type Recorder interface {
	RecordActivatorHold(ctx context.Context, component, outcome string, durationSeconds float64)
	RecordActivatorQueueDelta(ctx context.Context, component string, delta int64)
	RecordActivatorRaise(ctx context.Context, component string)
	RecordActivatorAsync(ctx context.Context, component, result string)
}

// broker is the hold-and-forward pipeline shared by the components in front of
// workloads: it holds requests until the capacity yields a target — raising
// cold workloads that can be raised, debounced — and proxies sync requests.
// Async delivery is deployment-shaped and lives with deploymentBroker.
type broker struct {
	rec Recorder
	// component names who is running this broker, so one metric series can carry
	// both without conflating a sandbox hold with a deployment cold start.
	component string

	mu        sync.Mutex
	lastRaise map[string]time.Time // key → last cold scale-up
	queued    map[string]int       // key → requests waiting for an endpoint
}

// newBroker creates a broker. rec (nilable) receives the domain metrics.
func newBroker(rec Recorder, component string) *broker {
	return &broker{
		rec:       rec,
		component: component,
		lastRaise: make(map[string]time.Time),
		queued:    make(map[string]int),
	}
}

// QueuedDepth reports how many requests are waiting for the key's first
// endpoint — the autoscaler's hold-up signal during a cold start.
func (b *broker) queuedDepth(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queued[key]
}

// Queued snapshots the waiting-request count per key.
func (b *broker) depths() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.queued))
	for key, n := range b.queued {
		if n > 0 {
			out[key] = n
		}
	}
	return out
}

// Sync holds for a target (bounded by hold) and proxies the request to it,
// preserving host as the workload's virtual host. The per-request 504
// timeout is enforced by the workload-sidecar, not here.
func (b *broker) sync(w http.ResponseWriter, r *http.Request, key, host string, hold time.Duration, c capacity) {
	target, err := b.await(r.Context(), key, hold, c)
	if err != nil {
		http.Error(w, "no serving capacity became ready", http.StatusServiceUnavailable)
		return
	}
	proxyTo(target, host).ServeHTTP(w, r)
}

// await polls capacity for a target until hold expires. A workload that can be
// raised and has no target (scaled to zero, or its last replica crashed/was
// evicted) is raised first: the broker owns 0→N, never waiting on an autoscaler
// tick. For capacity with nothing to raise, this is a wait and nothing else.
func (b *broker) await(ctx context.Context, key string, hold time.Duration, c capacity) (target *url.URL, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, hold)
	defer cancel()

	start := time.Now()
	b.mu.Lock()
	b.queued[key]++
	b.mu.Unlock()
	if b.rec != nil {
		b.rec.RecordActivatorQueueDelta(ctx, b.component, 1)
	}
	defer func() {
		b.mu.Lock()
		if b.queued[key]--; b.queued[key] <= 0 {
			delete(b.queued, key)
		}
		b.mu.Unlock()
		if b.rec != nil {
			b.rec.RecordActivatorQueueDelta(ctx, b.component, -1)
			outcome := "served"
			if err != nil {
				outcome = "timeout"
			}
			b.rec.RecordActivatorHold(ctx, b.component, outcome, time.Since(start).Seconds())
		}
	}()

	canRaise, _ := c.(riser)
	ticker := time.NewTicker(endpointPollInterval)
	defer ticker.Stop()
	for {
		if target, err := c.Target(waitCtx); err == nil && target != nil {
			return target, nil
		}
		if canRaise != nil {
			b.raise(waitCtx, key, canRaise)
		}
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

// raise requests a cold workload's scale-up, debounced per key so concurrent
// cold hits (and the poll loop) issue one write. Failures are logged, not
// returned — the hold carries on and the request fails with 503 only if
// nothing becomes ready in time.
func (b *broker) raise(ctx context.Context, key string, c riser) {
	b.mu.Lock()
	if time.Since(b.lastRaise[key]) < raiseDebounce {
		b.mu.Unlock()
		return
	}
	b.lastRaise[key] = time.Now()
	pruneStale(b.lastRaise, raiseDebounce)
	b.mu.Unlock()

	if b.rec != nil {
		b.rec.RecordActivatorRaise(ctx, b.component)
	}
	if err := c.Raise(ctx); err != nil {
		slog.Warn("Cold-start scale-up failed", "key", key, "error", err)
	}
}

// proxyTo builds a single-shot reverse proxy to the target endpoint,
// preserving the original Host for the workload.
func proxyTo(target *url.URL, host string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("Proxy error", "host", host, "target", target.String(), "error", err)
			http.Error(w, "upstream connection failed", http.StatusBadGateway)
		},
	}
}

// pruneMapThreshold bounds the per-workload bookkeeping maps: beyond it,
// stale entries (deleted deployments, retired revisions) are dropped so churn
// can't grow them without bound. Callers hold the map's lock.
const pruneMapThreshold = 1024

// pruneStale drops timestamp entries older than 100× their useful horizon.
func pruneStale(m map[string]time.Time, horizon time.Duration) {
	if len(m) < pruneMapThreshold {
		return
	}
	cutoff := time.Now().Add(-100 * horizon)
	for k, t := range m {
		if t.Before(cutoff) {
			delete(m, k)
		}
	}
}
