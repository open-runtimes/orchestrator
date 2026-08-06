package activator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/deployment"
	"orchestrator/internal/dispatcher"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResolver serves a single deployment at a fixed host.
type fakeResolver struct {
	spec *deployment.Request

	mu               sync.Mutex
	endpoints        []*url.URL
	scaleCalls       []int
	endpointsOnScale []*url.URL // revealed by Scale, simulating a cold start
}

func (f *fakeResolver) Resolve(_ context.Context, host string) (*deployment.Request, error) {
	if f.spec != nil && slices.Contains(f.spec.Hosts, host) {
		return f.spec, nil
	}
	return nil, apperrors.NotFound("deployment", host)
}

func (f *fakeResolver) Endpoints(context.Context, string) ([]*url.URL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.endpoints, nil
}

func (f *fakeResolver) Scale(_ context.Context, _ string, replicas int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scaleCalls = append(f.scaleCalls, replicas)
	if f.endpointsOnScale != nil {
		f.endpoints = f.endpointsOnScale
	}
	return nil
}

func (f *fakeResolver) scaled() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.scaleCalls...)
}

// captureQueue records dispatched events and signals on each Dispatch.
type captureQueue struct {
	mu     sync.Mutex
	events []*dispatcher.Event
	ch     chan struct{}
}

func newCaptureQueue() *captureQueue {
	return &captureQueue{ch: make(chan struct{}, 8)}
}

func (q *captureQueue) Dispatch(e *dispatcher.Event) error {
	q.mu.Lock()
	q.events = append(q.events, e)
	q.mu.Unlock()
	q.ch <- struct{}{}
	return nil
}

func (q *captureQueue) Stats() dispatcher.Stats     { return dispatcher.Stats{} }
func (q *captureQueue) Close(context.Context) error { return nil }
func (q *captureQueue) last() *dispatcher.Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.events) == 0 {
		return nil
	}
	return q.events[len(q.events)-1]
}

func newTestSpec(host string) *deployment.Request {
	return &deployment.Request{
		ID:                  "test",
		Hosts:               []string{host},
		Port:                8080,
		TimeoutSeconds:      5,
		StartTimeoutSeconds: 1,
	}
}

func testActivator(t *testing.T, backendHandler http.HandlerFunc, spec *deployment.Request) (*Activator, *captureQueue) {
	t.Helper()
	resolver := &fakeResolver{spec: spec}
	if backendHandler != nil {
		backend := httptest.NewServer(backendHandler)
		t.Cleanup(backend.Close)
		u, _ := url.Parse(backend.URL)
		resolver.endpoints = []*url.URL{u}
	}
	queue := newCaptureQueue()
	return New(resolver, queue, nil), queue
}

func TestSync_RoutesByHost(t *testing.T) {
	spec := newTestSpec("app.example.test")
	act, _ := testActivator(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "app.example.test" {
			t.Errorf("backend saw Host %q, want app.example.test", r.Host)
		}
		_, _ = io.WriteString(w, "hello")
	}, spec)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.test:8081/x", nil)
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q, want hello", rec.Body.String())
	}
}

func TestSync_UnknownHost404(t *testing.T) {
	act, _ := testActivator(t, nil, newTestSpec("app.example.test"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://other.example.test/", nil)
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSync_NoEndpoint503(t *testing.T) {
	spec := newTestSpec("app.example.test")
	act, _ := testActivator(t, nil, spec) // no backend → no endpoints

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.test/", nil)
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestSync_ColdStartRaisesAndServes(t *testing.T) {
	spec := newTestSpec("app.example.test")
	spec.Replicas = 2
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "warmed")
	}))
	t.Cleanup(backend.Close)
	u, _ := url.Parse(backend.URL)

	// No endpoints until Scale is called — a scaled-to-zero deployment.
	resolver := &fakeResolver{spec: spec, endpointsOnScale: []*url.URL{u}}
	act := New(resolver, newCaptureQueue(), nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.test/", nil)
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "warmed" {
		t.Fatalf("cold start: status=%d body=%q", rec.Code, rec.Body.String())
	}
	calls := resolver.scaled()
	if len(calls) != 1 || calls[0] != 2 {
		t.Fatalf("scale calls: want [2] (declared replicas, debounced), got %v", calls)
	}

	if act.QueuedDepth("test") != 0 {
		t.Fatal("queued gauge must drain back to zero after the cold start completes")
	}
}

func TestAsync_RequiresCallback(t *testing.T) {
	spec := newTestSpec("app.example.test")
	act, _ := testActivator(t, func(w http.ResponseWriter, r *http.Request) {}, spec)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.test/", strings.NewReader("{}"))
	req.Header.Set("Prefer", "respond-async")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// RFC 7240 preference tokens are case-insensitive: any casing of
// respond-async must take the async path, never silently serve sync.
func TestAsync_PreferValueCaseInsensitive(t *testing.T) {
	spec := newTestSpec("app.example.test")
	act, _ := testActivator(t, func(w http.ResponseWriter, r *http.Request) {}, spec)

	for _, prefer := range []string{"Respond-Async", "RESPOND-ASYNC"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.test/", strings.NewReader("{}"))
		req.Header.Set("Prefer", prefer)
		rec := httptest.NewRecorder()
		act.ServeHTTP(rec, req)

		// 400 (async requires a callback) proves the async path was taken; a
		// sync fallthrough would have proxied 200.
		if rec.Code != http.StatusBadRequest {
			t.Errorf("Prefer %q: status = %d, want 400 (the async path)", prefer, rec.Code)
		}
	}
}

func TestAsync_AcceptsAndDeliversCallback(t *testing.T) {
	spec := newTestSpec("app.example.test")
	spec.Callback = &deployment.Callback{URL: "http://callbacks.test/hook", Key: "k"}
	act, queue := testActivator(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("backend body = %q, want payload", body)
		}
		if r.Header.Get("Prefer") != "" {
			t.Error("Prefer header leaked to the workload")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "done")
	}, spec)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.test/", strings.NewReader("payload"))
	req.Header.Set("Prefer", "respond-async")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	invocationID := rec.Header().Get("X-Invocation-Id")
	if invocationID == "" {
		t.Fatal("missing X-Invocation-Id")
	}

	select {
	case <-queue.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no callback dispatched")
	}

	event := queue.last()
	if event.Destination != "http://callbacks.test/hook" {
		t.Fatalf("callback destination = %q", event.Destination)
	}
	if event.Payload.Type != "orchestrator.deployment.response" {
		t.Fatalf("event type = %q", event.Payload.Type)
	}
	if event.Payload.Data["invocationId"] != invocationID {
		t.Fatalf("invocationId mismatch: %v vs %s", event.Payload.Data["invocationId"], invocationID)
	}
	if event.Payload.Data["statusCode"] != http.StatusCreated {
		t.Fatalf("statusCode = %v, want 201", event.Payload.Data["statusCode"])
	}
	if event.Payload.Data["body"] != "done" {
		t.Fatalf("body = %v, want done", event.Payload.Data["body"])
	}
}

func TestAsync_ResponseTruncatedAtCap(t *testing.T) {
	spec := newTestSpec("app.example.test")
	spec.Callback = &deployment.Callback{URL: "http://callbacks.test/hook"}
	big := strings.Repeat("x", maxCallbackResponseBody+100)
	act, queue := testActivator(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, big)
	}, spec)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.test/", nil)
	req.Header.Set("Prefer", "respond-async")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	select {
	case <-queue.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no callback dispatched")
	}

	event := queue.last()
	body, _ := event.Payload.Data["body"].(string)
	if len(body) != maxCallbackResponseBody {
		t.Fatalf("body length = %d, want %d", len(body), maxCallbackResponseBody)
	}
	truncated, _ := event.Payload.Data["bodyTruncated"].(bool)
	if !truncated {
		t.Fatal("expected bodyTruncated = true")
	}
}
