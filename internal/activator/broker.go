package activator

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"orchestrator/internal/dispatcher"
	"orchestrator/pkg/cloudevent"
	"orchestrator/pkg/deployment"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// maxAsyncRequestBody bounds the buffered request body for async calls.
	maxAsyncRequestBody = 10 << 20 // 10 MiB
	// maxCallbackResponseBody bounds the response body shipped in the
	// .response callback; larger bodies are truncated and flagged.
	maxCallbackResponseBody = 1 << 20 // 1 MiB

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
// serving endpoint, and how to ask for one when there is none. Bound per request
// (to a deployment spec on Docker, a revision on Kubernetes, a sandbox token);
// the broker owns everything either side of it.
type capacity interface {
	// Target returns a reachable endpoint, or nil when none is ready yet.
	Target(ctx context.Context) (*url.URL, error)
	// Raise requests capacity for a cold workload. The broker debounces
	// calls per key; implementations own idempotence and success logging.
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

// broker is the hold-raise-forward pipeline shared by the components in front of
// workloads:
// it holds requests until the edge's capacity yields a target (raising cold
// workloads, debounced), proxies sync requests, and runs the async accept →
// forward → response-callback flow. Delivery is at-most-once: nothing is
// stored, X-Invocation-Id is a correlation id only.
type broker struct {
	queue  dispatcher.Queue
	source string // CloudEvents source
	rec    Recorder
	// component names who is running this broker, so one metric series can carry
	// both without conflating a sandbox hold with a deployment cold start.
	component string

	mu        sync.Mutex
	lastRaise map[string]time.Time // key → last cold scale-up
	queued    map[string]int       // key → requests waiting for an endpoint
}

// newBroker creates a broker. queue delivers async response callbacks; rec
// (nilable) receives the domain metrics.
func newBroker(queue dispatcher.Queue, rec Recorder, component string) *broker {
	return &broker{
		queue:     queue,
		source:    "orchestrator/deployments",
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
// timeout is enforced by the deployments-sidecar, not here.
func (b *broker) sync(w http.ResponseWriter, r *http.Request, key, host string, hold time.Duration, c capacity) {
	target, err := b.await(r.Context(), key, hold, c)
	if err != nil {
		http.Error(w, "no serving capacity became ready", http.StatusServiceUnavailable)
		return
	}
	proxyTo(target, host).ServeHTTP(w, r)
}

// Async buffers the request, responds 202 immediately, and delivers the
// eventual response to the deployment's callback as a CloudEvent. hold
// bounds the wait for the first endpoint; spec.TimeoutSeconds extends the
// total forward window.
func (b *broker) async(w http.ResponseWriter, r *http.Request, key, host string, spec *deployment.Request, hold time.Duration, c capacity) {
	if spec.Callback == nil || spec.Callback.URL == "" {
		http.Error(w, "async requires a callback on the deployment", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAsyncRequestBody+1))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if len(body) > maxAsyncRequestBody {
		http.Error(w, "async request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Honor a caller-supplied correlation id so the callback can be tied back
	// to the caller's own record; generate one when absent. It's a correlation
	// id only — never stored, no uniqueness enforced.
	invocationID := r.Header.Get("X-Invocation-Id")
	if invocationID == "" {
		invocationID = newInvocationID()
	}
	req := cloneForForward(r, host, body)

	w.Header().Set("X-Invocation-Id", invocationID)
	w.WriteHeader(http.StatusAccepted)

	go b.forwardAsync(req, key, spec, invocationID, hold, c)
}

// forwardAsync executes the buffered request against a ready endpoint and
// dispatches the response callback.
func (b *broker) forwardAsync(r *http.Request, key string, spec *deployment.Request, invocationID string, hold time.Duration, c capacity) {
	ctx, cancel := context.WithTimeout(context.Background(),
		hold+time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()

	target, err := b.await(ctx, key, hold, c)
	if err != nil {
		b.dispatchResponse(spec, invocationID, r, 0, 0, nil, false, "no serving capacity became ready")
		return
	}

	fwd := r.Clone(ctx)
	fwd.URL.Scheme = target.Scheme
	fwd.URL.Host = target.Host
	fwd.RequestURI = ""

	// Time the workload round-trip only (excludes the cold-start hold above),
	// so durationSeconds is the request's own processing time.
	start := time.Now()
	resp, err := http.DefaultClient.Do(fwd)
	elapsed := time.Since(start)
	if err != nil {
		slog.Warn("Async forward failed", "key", key, "invocationId", invocationID, "error", err)
		b.dispatchResponse(spec, invocationID, r, 0, 0, nil, false, "forward failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxCallbackResponseBody+1))
	if err != nil {
		b.dispatchResponse(spec, invocationID, r, elapsed, resp.StatusCode, nil, false, "failed to read response: "+err.Error())
		return
	}
	truncated := false
	if len(respBody) > maxCallbackResponseBody {
		respBody = respBody[:maxCallbackResponseBody]
		truncated = true
	}
	b.dispatchResponse(spec, invocationID, r, elapsed, resp.StatusCode, respBody, truncated, "")
}

// dispatchResponse emits the orchestrator.deployment.response CloudEvent. The
// original request's method, path, and headers are echoed back so a consumer
// can reconstruct its record from the callback alone — request headers double
// as a caller-defined metadata channel that round-trips.
func (b *broker) dispatchResponse(spec *deployment.Request, invocationID string, r *http.Request, duration time.Duration, status int, body []byte, truncated bool, errMsg string) {
	data := map[string]any{
		"deploymentId":  spec.ID,
		"invocationId":  invocationID,
		"requestMethod": r.Method,
	}
	// Bound the echoed path+query (URIs are ASCII, so a byte cut is safe) so a
	// long request target can't push the callback past a receiver/proxy limit.
	path := r.URL.RequestURI()
	if len(path) > maxEchoedPathBytes {
		path = path[:maxEchoedPathBytes]
		data["requestPathTruncated"] = true
	}
	data["requestPath"] = path
	if headers, truncated := echoHeaders(r.Header); headers != nil {
		data["requestHeaders"] = headers
	} else if truncated {
		data["requestHeadersTruncated"] = true
	}
	if duration > 0 {
		data["durationSeconds"] = duration.Seconds()
	}
	if status > 0 {
		data["statusCode"] = status
	}
	if body != nil {
		// JSON strings must be valid UTF-8 — Go silently replaces bad bytes
		// with U+FFFD, corrupting binary payloads. Base64 those instead and
		// say so.
		if utf8.Valid(body) {
			data["body"] = string(body)
		} else {
			data["body"] = base64.StdEncoding.EncodeToString(body)
			data["bodyEncoding"] = "base64"
		}
		data["bodyTruncated"] = truncated
	}
	if errMsg != "" {
		data["error"] = errMsg
	}

	if b.rec != nil {
		result := "delivered"
		if errMsg != "" {
			result = "failed"
		}
		b.rec.RecordActivatorAsync(context.Background(), b.component, result)
	}
	event := cloudevent.New("orchestrator.deployment.response", b.source, spec.ID, invocationID, data)
	if err := b.queue.Dispatch(&dispatcher.Event{
		Payload:     event,
		Destination: spec.Callback.URL,
		SigningKey:  spec.Callback.Key,
	}); err != nil {
		slog.Warn("Failed to dispatch async response", "deploymentId", spec.ID, "invocationId", invocationID, "error", err)
	}
}

// await polls capacity for a target until hold expires. A cold workload (no
// target — scaled to zero, or its last replica crashed/was evicted) is
// raised first: the broker owns 0→N, never waiting on an autoscaler tick.
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

	ticker := time.NewTicker(endpointPollInterval)
	defer ticker.Stop()
	for {
		if target, err := c.Target(waitCtx); err == nil && target != nil {
			return target, nil
		}
		b.raise(waitCtx, key, c)
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
func (b *broker) raise(ctx context.Context, key string, c capacity) {
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

// cloneForForward makes a detached copy of the request with a buffered body.
// isSensitiveHeader reports headers never echoed in the response event: the
// callback can be logged or stored by the consumer, so credentials meant for
// the request path must not travel with it.
func isSensitiveHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie":
		return true
	default:
		return false
	}
}

// maxEchoedHeaderBytes and maxEchoedPathBytes bound the echoed request metadata
// so a request with large headers or a long path can't produce a callback that
// exceeds a receiver/proxy limit and fails delivery.
const (
	maxEchoedHeaderBytes = 16 << 10 // 16 KiB
	maxEchoedPathBytes   = 4 << 10  // 4 KiB
)

// echoHeaders copies request headers for the response event, preserving the
// multi-value shape (joining is lossy for repeated values and invalid for
// Set-Cookie) and dropping credential-bearing headers. Over the size cap it
// returns (nil, true) so the caller ships a truncation flag instead of an
// undeliverable event.
func echoHeaders(h http.Header) (map[string][]string, bool) {
	out := make(map[string][]string, len(h))
	size := 0
	for name, values := range h {
		if isSensitiveHeader(name) {
			continue
		}
		for _, v := range values {
			size += len(name) + len(v)
		}
		out[name] = values
	}
	if size > maxEchoedHeaderBytes {
		return nil, true
	}
	return out, false
}

func cloneForForward(r *http.Request, host string, body []byte) *http.Request {
	req := r.Clone(context.Background())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Host = host
	req.Header.Del("Prefer")
	req.Header.Del("X-Invocation-Id")
	return req
}

func newInvocationID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
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
