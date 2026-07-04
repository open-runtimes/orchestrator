// Package activator is the deployments data-plane edge for Phase 1: it is
// always on the request path, routing by Host to a deployment's ready proxy
// endpoint, and owning the sync/async split (Prefer: respond-async → 202 +
// callback). From Phase 3 the gateway takes the warm path and this component
// only buffers cold/async traffic. See docs/design/deployments-activator.md.
package activator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"orchestrator/internal/dispatcher"
	"orchestrator/pkg/cloudevent"
	"orchestrator/pkg/deployment"
	"strings"
	"sync"
	"time"
)

const (
	// maxAsyncRequestBody bounds the buffered request body for async calls.
	maxAsyncRequestBody = 10 << 20 // 10 MiB
	// maxCallbackResponseBody bounds the response body shipped in the
	// .response callback; larger bodies are truncated and flagged.
	maxCallbackResponseBody = 1 << 20 // 1 MiB

	endpointPollInterval = 100 * time.Millisecond

	// resolveTTL bounds how long a host→spec resolution is reused on the data
	// path. Spec changes (or deletes) take up to this long to be seen here.
	resolveTTL = time.Second
)

// Resolver maps a request host to its deployment spec and supplies ready
// endpoints. Implemented by deployment.Service.
type Resolver interface {
	Resolve(ctx context.Context, host string) (*deployment.Request, error)
	Endpoints(ctx context.Context, id string) ([]*url.URL, error)
}

// Activator routes data-plane traffic by Host.
type Activator struct {
	resolver Resolver
	queue    dispatcher.Queue
	source   string // CloudEvents source

	mu    sync.Mutex
	cache map[string]resolveEntry // host → spec, TTL-bounded
}

type resolveEntry struct {
	spec    *deployment.Request
	expires time.Time
}

// New creates an Activator. queue delivers async response callbacks.
func New(resolver Resolver, queue dispatcher.Queue) *Activator {
	return &Activator{
		resolver: resolver,
		queue:    queue,
		source:   "orchestrator/deployments",
		cache:    make(map[string]resolveEntry),
	}
}

// resolve is the cached host→spec lookup for the data path; misses fall
// through to the Resolver (a full backend scan).
func (a *Activator) resolve(ctx context.Context, host string) (*deployment.Request, error) {
	a.mu.Lock()
	entry, ok := a.cache[host]
	a.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.spec, nil
	}

	spec, err := a.resolver.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cache[host] = resolveEntry{spec: spec, expires: time.Now().Add(resolveTTL)}
	a.mu.Unlock()
	return spec, nil
}

// ServeHTTP implements the data plane: resolve Host → deployment, then either
// proxy synchronously or accept for async delivery.
func (a *Activator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	spec, err := a.resolve(r.Context(), host)
	if err != nil {
		http.Error(w, "no deployment for host "+host, http.StatusNotFound)
		return
	}

	// Exact-literal match by design; combined RFC 7240 forms are not recognized.
	if r.Header.Get("Prefer") == "respond-async" {
		a.serveAsync(w, r, spec)
		return
	}
	a.serveSync(w, r, spec)
}

// serveSync waits for a ready endpoint (bounded by the deployment's
// responseStartTimeout) and proxies the request to it. The per-request 504
// timeout is enforced by the deployments-sidecar, not here.
func (a *Activator) serveSync(w http.ResponseWriter, r *http.Request, spec *deployment.Request) {
	target, err := a.waitForEndpoint(r.Context(), spec)
	if err != nil {
		http.Error(w, "no serving capacity became ready", http.StatusServiceUnavailable)
		return
	}
	proxyTo(target, spec.Host).ServeHTTP(w, r)
}

// serveAsync buffers the request, responds 202 immediately, and delivers the
// eventual response to the deployment's callback as a CloudEvent. Delivery is
// at-most-once: nothing is stored, X-Invocation-Id is a correlation id only.
func (a *Activator) serveAsync(w http.ResponseWriter, r *http.Request, spec *deployment.Request) {
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

	invocationID := newInvocationID()
	req := cloneForForward(r, spec.Host, body)

	w.Header().Set("X-Invocation-Id", invocationID)
	w.WriteHeader(http.StatusAccepted)

	go a.forwardAsync(req, spec, invocationID)
}

// forwardAsync executes the buffered request against a ready endpoint and
// dispatches the response callback.
func (a *Activator) forwardAsync(r *http.Request, spec *deployment.Request, invocationID string) {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(spec.ResponseStartTimeoutSeconds+spec.TimeoutSeconds)*time.Second)
	defer cancel()
	logger := slog.With("deploymentId", spec.ID, "invocationId", invocationID)

	target, err := a.waitForEndpoint(ctx, spec)
	if err != nil {
		a.dispatchResponse(spec, invocationID, 0, nil, false, "no serving capacity became ready")
		return
	}

	fwd := r.Clone(ctx)
	fwd.URL.Scheme = target.Scheme
	fwd.URL.Host = target.Host
	fwd.RequestURI = ""

	resp, err := http.DefaultClient.Do(fwd)
	if err != nil {
		logger.Warn("Async forward failed", "error", err)
		a.dispatchResponse(spec, invocationID, 0, nil, false, "forward failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxCallbackResponseBody+1))
	if err != nil {
		a.dispatchResponse(spec, invocationID, resp.StatusCode, nil, false, "failed to read response: "+err.Error())
		return
	}
	truncated := false
	if len(respBody) > maxCallbackResponseBody {
		respBody = respBody[:maxCallbackResponseBody]
		truncated = true
	}
	a.dispatchResponse(spec, invocationID, resp.StatusCode, respBody, truncated, "")
}

// dispatchResponse emits the orchestrator.deployment.response CloudEvent.
func (a *Activator) dispatchResponse(spec *deployment.Request, invocationID string, status int, body []byte, truncated bool, errMsg string) {
	data := map[string]any{
		"deploymentId": spec.ID,
		"invocationId": invocationID,
	}
	if status > 0 {
		data["statusCode"] = status
	}
	if body != nil {
		data["body"] = string(body)
		data["bodyTruncated"] = truncated
	}
	if errMsg != "" {
		data["error"] = errMsg
	}

	event := cloudevent.New("orchestrator.deployment.response", a.source, spec.ID, invocationID, data)
	if err := a.queue.Dispatch(&dispatcher.Event{
		Payload:     event,
		Destination: spec.Callback.URL,
		SigningKey:  spec.Callback.Key,
	}); err != nil {
		slog.Warn("Failed to dispatch async response", "deploymentId", spec.ID, "invocationId", invocationID, "error", err)
	}
}

// waitForEndpoint polls for a ready endpoint until the deployment's
// responseStartTimeout expires.
func (a *Activator) waitForEndpoint(ctx context.Context, spec *deployment.Request) (*url.URL, error) {
	deadline := time.Duration(spec.ResponseStartTimeoutSeconds) * time.Second
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for {
		endpoints, err := a.resolver.Endpoints(waitCtx, spec.ID)
		if err == nil && len(endpoints) > 0 {
			return endpoints[0], nil
		}
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case <-time.After(endpointPollInterval):
		}
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
func cloneForForward(r *http.Request, host string, body []byte) *http.Request {
	req := r.Clone(context.Background())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Host = host
	req.Header.Del("Prefer")
	return req
}

func hostOnly(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i != -1 && !strings.Contains(hostport[i:], "]") {
		return hostport[:i]
	}
	return hostport
}

func newInvocationID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
