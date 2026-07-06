package activator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"orchestrator/pkg/deployment"
	"sync"
	"testing"
	"time"
)

// fakeCapacity is a Capacity whose target appears after a fixed number of
// Target attempts — attempt 0 means warm from the start.
type fakeCapacity struct {
	mu         sync.Mutex
	target     *url.URL
	readyAfter int // Target calls before the target is revealed
	attempts   int
	raises     int
	raiseErr   error
}

func (c *fakeCapacity) Target(context.Context) (*url.URL, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if c.attempts > c.readyAfter {
		return c.target, nil
	}
	return nil, nil //nolint:nilnil // nil,nil is the Capacity contract for "none ready yet"
}

func (c *fakeCapacity) Raise(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raises++
	return c.raiseErr
}

func (c *fakeCapacity) raiseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raises
}

func newDataRequest(t *testing.T, method string, body *bytes.Reader) *http.Request {
	t.Helper()
	var r io.Reader = http.NoBody
	if body != nil {
		r = body
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://h/", r)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func brokerSpec() *deployment.Request {
	return &deployment.Request{
		ID:             "dep",
		Host:           "dep.example.test",
		TimeoutSeconds: 5,
		Callback:       &deployment.Callback{URL: "http://hooks.example.test/cb"},
	}
}

func waitEvent(t *testing.T, q *captureQueue) map[string]any {
	t.Helper()
	select {
	case <-q.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no callback dispatched")
	}
	return q.last().Payload.Data
}

func TestBrokerSyncProxiesWhenWarm(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-Host", r.Host)
		_, _ = w.Write([]byte("pong"))
	}))
	defer backend.Close()

	b := NewBroker(newCaptureQueue())
	c := &fakeCapacity{target: mustURL(t, backend.URL)}

	rec := httptest.NewRecorder()
	b.Sync(rec, newDataRequest(t, http.MethodGet, nil), "dep", "dep.example.test", time.Second, c)

	if rec.Code != http.StatusOK || rec.Body.String() != "pong" {
		t.Fatalf("got %d %q, want 200 pong", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Served-Host"); got != "dep.example.test" {
		t.Errorf("workload saw Host %q, want the virtual host", got)
	}
	if c.raiseCount() != 0 {
		t.Errorf("warm target raised %d times, want 0", c.raiseCount())
	}
}

func TestBrokerColdHoldRaisesOnceThen503(t *testing.T) {
	b := NewBroker(newCaptureQueue())
	c := &fakeCapacity{readyAfter: 1 << 30} // never ready

	const concurrent = 5
	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := range concurrent {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			b.Sync(rec, newDataRequest(t, http.MethodGet, nil), "dep", "h", 300*time.Millisecond, c)
			codes[i] = rec.Code
		})
	}

	// While the hold is in flight, the queued gauge must expose it.
	deadline := time.Now().Add(time.Second)
	for b.QueuedDepth("dep") < concurrent && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := b.QueuedDepth("dep"); got != concurrent {
		t.Errorf("QueuedDepth = %d during hold, want %d", got, concurrent)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusServiceUnavailable {
			t.Errorf("request %d: got %d, want 503", i, code)
		}
	}
	// Concurrent cold hits and every poll tick funnel through one debounced raise.
	if got := c.raiseCount(); got != 1 {
		t.Errorf("raise called %d times, want 1 (debounced)", got)
	}
	if got := b.QueuedDepth("dep"); got != 0 {
		t.Errorf("QueuedDepth = %d after hold, want 0", got)
	}
}

func TestBrokerColdStartServesAfterRaise(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("warmed"))
	}))
	defer backend.Close()

	b := NewBroker(newCaptureQueue())
	c := &fakeCapacity{target: mustURL(t, backend.URL), readyAfter: 2}

	rec := httptest.NewRecorder()
	b.Sync(rec, newDataRequest(t, http.MethodGet, nil), "dep", "h", 5*time.Second, c)

	if rec.Code != http.StatusOK || rec.Body.String() != "warmed" {
		t.Fatalf("got %d %q, want 200 warmed", rec.Code, rec.Body.String())
	}
	if c.raiseCount() == 0 {
		t.Error("cold start never raised")
	}
}

func TestBrokerAsyncRequiresCallback(t *testing.T) {
	b := NewBroker(newCaptureQueue())
	spec := brokerSpec()
	spec.Callback = nil

	rec := httptest.NewRecorder()
	b.Async(rec, newDataRequest(t, http.MethodPost, nil), "dep", "h", spec, time.Second, &fakeCapacity{})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestBrokerAsyncRejectsOversizedBody(t *testing.T) {
	b := NewBroker(newCaptureQueue())

	body := bytes.NewReader(make([]byte, maxAsyncRequestBody+1))
	rec := httptest.NewRecorder()
	b.Async(rec, newDataRequest(t, http.MethodPost, body), "dep", "h", brokerSpec(), time.Second, &fakeCapacity{})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

func TestBrokerAsyncDeliversResponseCallback(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("done"))
	}))
	defer backend.Close()

	queue := newCaptureQueue()
	b := NewBroker(queue)
	c := &fakeCapacity{target: mustURL(t, backend.URL)}

	rec := httptest.NewRecorder()
	b.Async(rec, newDataRequest(t, http.MethodPost, strings2reader("hi")), "dep", "h", brokerSpec(), time.Second, c)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
	invocationID := rec.Header().Get("X-Invocation-Id")
	if invocationID == "" {
		t.Fatal("missing X-Invocation-Id")
	}

	data := waitEvent(t, queue)
	if data["deploymentId"] != "dep" || data["invocationId"] != invocationID {
		t.Errorf("callback correlation = %v/%v, want dep/%s", data["deploymentId"], data["invocationId"], invocationID)
	}
	if data["statusCode"] != http.StatusCreated || data["body"] != "done" {
		t.Errorf("callback carried %v %q, want 201 done", data["statusCode"], data["body"])
	}
	if got, ok := data["bodyTruncated"].(bool); !ok || got {
		t.Errorf("bodyTruncated = %v, want false", data["bodyTruncated"])
	}
}

// Binary (non-UTF-8) response bodies must survive the JSON callback intact —
// the rule the two edges drifted apart on before the broker unified them.
func TestBrokerAsyncCallbackBase64ForBinaryBody(t *testing.T) {
	binary := []byte{0xff, 0xfe, 0x00, 0x80, 0x81}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(binary)
	}))
	defer backend.Close()

	queue := newCaptureQueue()
	b := NewBroker(queue)

	rec := httptest.NewRecorder()
	b.Async(rec, newDataRequest(t, http.MethodPost, nil), "dep", "h", brokerSpec(), time.Second,
		&fakeCapacity{target: mustURL(t, backend.URL)})

	data := waitEvent(t, queue)
	if data["bodyEncoding"] != "base64" {
		t.Fatalf("bodyEncoding = %v, want base64", data["bodyEncoding"])
	}
	decoded, err := base64.StdEncoding.DecodeString(data["body"].(string))
	if err != nil || !bytes.Equal(decoded, binary) {
		t.Errorf("decoded body = %v (%v), want original binary", decoded, err)
	}
	// The whole event must still be JSON-encodable without corruption.
	if _, err := json.Marshal(data); err != nil {
		t.Errorf("callback data not JSON-encodable: %v", err)
	}
}

func TestBrokerAsyncCallbackTruncatesLargeBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxCallbackResponseBody+512))
	}))
	defer backend.Close()

	queue := newCaptureQueue()
	b := NewBroker(queue)

	rec := httptest.NewRecorder()
	b.Async(rec, newDataRequest(t, http.MethodPost, nil), "dep", "h", brokerSpec(), time.Second,
		&fakeCapacity{target: mustURL(t, backend.URL)})

	data := waitEvent(t, queue)
	if got, ok := data["bodyTruncated"].(bool); !ok || !got {
		t.Errorf("bodyTruncated = %v, want true", data["bodyTruncated"])
	}
	if got := len(data["body"].(string)); got != maxCallbackResponseBody {
		t.Errorf("body length = %d, want %d", got, maxCallbackResponseBody)
	}
}

func TestBrokerAsyncReportsHoldTimeout(t *testing.T) {
	queue := newCaptureQueue()
	b := NewBroker(queue)

	rec := httptest.NewRecorder()
	b.Async(rec, newDataRequest(t, http.MethodPost, nil), "dep", "h", brokerSpec(), 200*time.Millisecond,
		&fakeCapacity{readyAfter: 1 << 30})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (failure arrives on the callback)", rec.Code)
	}
	data := waitEvent(t, queue)
	if data["error"] != "no serving capacity became ready" {
		t.Errorf("callback error = %v, want hold-timeout message", data["error"])
	}
	if _, ok := data["statusCode"]; ok {
		t.Error("statusCode present on a request that never forwarded")
	}
}

func strings2reader(s string) *bytes.Reader { return bytes.NewReader([]byte(s)) }
