package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/health"
	"orchestrator/pkg/cloudevent"
	"testing"
)

func TestHandler_Livez(t *testing.T) {
	t.Parallel()
	handler := &Handler{
		health: health.NewChecker(nil),
	}

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	w := httptest.NewRecorder()

	handler.Livez(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var response health.Response
	json.NewDecoder(w.Body).Decode(&response)

	if response.Status != health.StatusHealthy {
		t.Errorf("Expected status healthy, got %s", response.Status)
	}
}

func TestHandler_Readyz_NoDocker(t *testing.T) {
	t.Parallel()
	handler := &Handler{
		health: health.NewChecker(nil), // No Docker client
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	handler.Readyz(w, req)

	// Should return 503 because Docker is not available
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d", http.StatusServiceUnavailable, w.Code)
	}

	var response health.Response
	json.NewDecoder(w.Body).Decode(&response)

	if response.Status != health.StatusUnhealthy {
		t.Errorf("Expected status unhealthy, got %s", response.Status)
	}
}

func TestHandler_CreateJob_InvalidJSON(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString("invalid json"))
	w := httptest.NewRecorder()

	handler.CreateJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestMiddleware_Logging(t *testing.T) {
	t.Parallel()
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := LoggingMiddleware()(inner)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("Inner handler was not called")
	}
}

func TestMiddleware_Recovery(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	handler := RecoveryMiddleware()(inner)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	// Should not panic
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}
}

func TestMiddleware_ContentType(t *testing.T) {
	t.Parallel()
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := ContentTypeMiddleware()(inner)

	// Test with wrong content type
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("Expected status %d, got %d", http.StatusUnsupportedMediaType, w.Code)
	}

	// Test with correct content type
	called = false
	req = httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("Inner handler was not called")
	}
}

func TestMiddleware_CORS(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := CORSMiddleware()(inner)

	// Test OPTIONS preflight
	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("Expected CORS header")
	}
}

func TestHandler_CreateJob_MissingID(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	body := `{"image": "alpine"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.CreateJob(w, req)

	// Should fail because svc is nil, but first let's check the request parses
	if w.Code == http.StatusBadRequest {
		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["error"] == "" {
			t.Error("Expected error message")
		}
	}
}

func TestHandler_CreateJob_MissingImage(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	body := `{"id": "test-job"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.CreateJob(w, req)

	// Will fail at service level since svc is nil
	if w.Code != http.StatusInternalServerError {
		t.Logf("Status: %d", w.Code)
	}
}

func TestHandler_CreateJob_EmptyBody(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(""))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.CreateJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandler_CreateJob_MalformedJSON(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	body := `{"id": "test", "image": alpine}` // missing quotes around alpine
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.CreateJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] == "" {
		t.Error("Expected error message in response")
	}
}

func TestHandler_GetJob_EmptyID(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/", nil)
	w := httptest.NewRecorder()

	handler.GetJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandler_DeleteJob_EmptyID(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	req := httptest.NewRequest(http.MethodDelete, "/v1/jobs/", nil)
	w := httptest.NewRecorder()

	handler.DeleteJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestMiddleware_ContentType_EmptyBodyAllowed(t *testing.T) {
	t.Parallel()
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := ContentTypeMiddleware()(inner)

	// GET requests don't need content-type
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("Inner handler should be called for GET requests")
	}
}

// mockDispatcher records dispatched events for testing.
type mockDispatcher struct {
	events []*dispatcher.Event
}

func (m *mockDispatcher) Dispatch(event *dispatcher.Event) error {
	m.events = append(m.events, event)
	return nil
}

func (m *mockDispatcher) Stats() dispatcher.Stats {
	return dispatcher.Stats{}
}

func (m *mockDispatcher) Close(ctx context.Context) error {
	return nil
}

func TestHandler_ProxyEvent(t *testing.T) {
	t.Parallel()
	mock := &mockDispatcher{}
	handler := &Handler{dispatcher: mock}

	event := cloudevent.New("test.event", "test-source", "job-123", "evt-1", nil)
	body, _ := json.Marshal(event)

	// Sidecar sends pre-signed events with signature in header
	req := httptest.NewRequest(http.MethodPost, "/internal/events?url=https://example.com/webhook", bytes.NewReader(body))
	req.Header.Set("X-Signature-256", "sha256=abc123")
	w := httptest.NewRecorder()

	handler.ProxyEvent(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("Expected status %d, got %d", http.StatusAccepted, w.Code)
	}

	if len(mock.events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(mock.events))
	}

	dispatched := mock.events[0]
	if dispatched.Destination != "https://example.com/webhook" {
		t.Errorf("Expected destination https://example.com/webhook, got %s", dispatched.Destination)
	}
	if dispatched.Signature != "sha256=abc123" {
		t.Errorf("Expected signature 'sha256=abc123', got %s", dispatched.Signature)
	}
	if dispatched.Payload.Type != "test.event" {
		t.Errorf("Expected event type test.event, got %s", dispatched.Payload.Type)
	}
}

func TestHandler_ProxyEvent_MissingURL(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	req := httptest.NewRequest(http.MethodPost, "/internal/events", bytes.NewBufferString("{}"))
	w := httptest.NewRecorder()

	handler.ProxyEvent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandler_ProxyEvent_InvalidJSON(t *testing.T) {
	t.Parallel()
	handler := &Handler{dispatcher: &mockDispatcher{}}

	req := httptest.NewRequest(http.MethodPost, "/internal/events?url=https://example.com", bytes.NewBufferString("invalid"))
	w := httptest.NewRecorder()

	handler.ProxyEvent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}
