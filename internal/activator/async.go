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
	"orchestrator/internal/cloudevent"
	"orchestrator/internal/deployment"
	"orchestrator/internal/dispatcher"
	"time"
	"unicode/utf8"
)

const (
	// maxAsyncRequestBody bounds the buffered request body for async calls.
	maxAsyncRequestBody = 10 << 20 // 10 MiB
	// maxCallbackResponseBody bounds the response body shipped in the
	// .response callback; larger bodies are truncated and flagged.
	maxCallbackResponseBody = 1 << 20 // 1 MiB
)

// deploymentBroker adds async delivery to the shared hold-and-forward broker.
// It is deployment-shaped on purpose: the callback and the event triple come off
// a deployment spec, and the response is dispatched as an
// orchestrator.deployment.response CloudEvent. Delivery is at-most-once:
// nothing is stored, X-Invocation-Id is a correlation id only.
//
// Only the components serving deployments construct one, so a broker with no
// queue cannot be asked to deliver a callback — the sandbox proxy holds a plain
// broker, since async execution inside a sandbox belongs to the image's own
// contract, not to us.
type deploymentBroker struct {
	*broker
	queue  dispatcher.Queue
	source string // CloudEvents source
}

// newDeploymentBroker creates a broker that can also deliver async responses.
// queue delivers those callbacks; rec (nilable) receives the domain metrics.
// Only the deployments activator runs one, so it names itself.
func newDeploymentBroker(queue dispatcher.Queue, rec Recorder) *deploymentBroker {
	return &deploymentBroker{
		broker: newBroker(rec, componentActivator),
		queue:  queue,
		source: "orchestrator/deployments",
	}
}

// Async buffers the request, responds 202 immediately, and delivers the
// eventual response to the deployment's callback as a CloudEvent. hold
// bounds the wait for the first endpoint; spec.TimeoutSeconds extends the
// total forward window.
func (b *deploymentBroker) async(w http.ResponseWriter, r *http.Request, key, host string, spec *deployment.Request, hold time.Duration, c capacity) {
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
func (b *deploymentBroker) forwardAsync(r *http.Request, key string, spec *deployment.Request, invocationID string, hold time.Duration, c capacity) {
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
func (b *deploymentBroker) dispatchResponse(spec *deployment.Request, invocationID string, r *http.Request, duration time.Duration, status int, body []byte, truncated bool, errMsg string) {
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

// cloneForForward makes a detached copy of the request with a buffered body.
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
