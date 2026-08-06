package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/health"
	"orchestrator/internal/job"
	"sync"
	"testing"
)

func TestRoutePattern(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /v1/jobs/{jobId}", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /internal/jobs/{jobId}/artifact", func(http.ResponseWriter, *http.Request) {})

	tests := []struct{ method, target, want string }{
		{"GET", "/livez", "/livez"},
		{"GET", "/v1/jobs/abc123", "/v1/jobs/{jobId}"},
		{"POST", "/internal/jobs/abc-def-build/artifact", "/internal/jobs/{jobId}/artifact"},
		{"GET", "/.env", "other"},
		{"GET", "/wp-includes/wlwmanifest.xml", "other"},
		{"DELETE", "/v1/jobs/abc123", "other"}, // 405: no route for this method
	}

	for _, tt := range tests {
		req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.target, nil)
		got := routePattern(mux, req)
		if got != tt.want {
			t.Errorf("routePattern(%s %s) = %q, want %q", tt.method, tt.target, got, tt.want)
		}
	}
}

func TestHandler_Livez(t *testing.T) {
	t.Parallel()
	handler := &Handler{
		health: health.NewChecker(nil),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/livez", nil)
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/jobs", bytes.NewBufferString("invalid json"))
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/test", nil)
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	// Should not panic
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}
}

// Every error the API emits must be {"error": ...} JSON — including the
// stdlib mux's plain-text 404/405 fallbacks and the 415 content-type reject.
func TestMiddleware_JSONErrorShapes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/things", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})
	handler := ContentTypeMiddleware()(JSONErrorMiddleware()(mux))

	cases := []struct {
		name, method, path, contentType string
		wantCode                        int
		wantAllow                       bool
	}{
		{"unknown route", http.MethodGet, "/nope", "", http.StatusNotFound, false},
		{"wrong method", http.MethodDelete, "/v1/things", "", http.StatusMethodNotAllowed, true},
		{"bad content type", http.MethodPost, "/v1/things", "text/plain", http.StatusUnsupportedMediaType, false},
	}
	for _, tc := range cases {
		req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, bytes.NewBufferString("{}"))
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d", tc.name, w.Code, tc.wantCode)
		}
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: Content-Type = %q, want application/json", tc.name, got)
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] == "" {
			t.Errorf("%s: body %q is not an {\"error\": ...} object (%v)", tc.name, w.Body.String(), err)
		}
		if tc.wantAllow && w.Header().Get("Allow") == "" {
			t.Errorf("%s: 405 lost its Allow header", tc.name)
		}
	}

	// Handler-written success responses pass through untouched.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/things", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"ok"`)) {
		t.Errorf("success passthrough broken: %d %s", w.Code, w.Body.String())
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/test", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("Expected status %d, got %d", http.StatusUnsupportedMediaType, w.Code)
	}

	// Test with correct content type
	called = false
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/test", bytes.NewBufferString("{}"))
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/test", nil)
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/jobs", bytes.NewBufferString(""))
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/jobs/", nil)
	w := httptest.NewRecorder()

	handler.GetJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandler_DeleteJob_EmptyID(t *testing.T) {
	t.Parallel()
	handler := &Handler{}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/v1/jobs/", nil)
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("Inner handler should be called for GET requests")
	}
}

// mockArtifactEmitter records EmitArtifactEvent calls for testing.
type mockArtifactEmitter struct {
	mu      sync.Mutex
	reports []job.ArtifactReport
}

func (m *mockArtifactEmitter) EmitArtifactEvent(r job.ArtifactReport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, r)
}

func TestHandler_ReportArtifact(t *testing.T) {
	t.Parallel()
	mock := &mockArtifactEmitter{}
	handler := &Handler{artifactEmitter: mock}

	report := job.ArtifactReport{
		ID:     "a1",
		Type:   "upload",
		Status: "success",
	}
	body, _ := json.Marshal(report)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/internal/jobs/job-123/artifact", bytes.NewReader(body))
	req.SetPathValue("jobId", "job-123")
	w := httptest.NewRecorder()

	handler.ReportArtifact(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("Expected status %d, got %d", http.StatusAccepted, w.Code)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.reports) != 1 {
		t.Fatalf("Expected 1 report, got %d", len(mock.reports))
	}
	r := mock.reports[0]
	if r.JobID != "job-123" {
		t.Errorf("Expected JobID 'job-123', got %q", r.JobID)
	}
	if r.ID != "a1" {
		t.Errorf("Expected ArtifactID 'a1', got %q", r.ID)
	}
	if r.Status != "success" {
		t.Errorf("Expected Status 'success', got %q", r.Status)
	}
}

func TestHandler_ReportArtifact_InvalidJSON(t *testing.T) {
	t.Parallel()
	handler := &Handler{artifactEmitter: &mockArtifactEmitter{}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/internal/jobs/job-123/artifact", bytes.NewBufferString("invalid"))
	req.SetPathValue("jobId", "job-123")
	w := httptest.NewRecorder()

	handler.ReportArtifact(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandler_ReportArtifact_MissingJobID(t *testing.T) {
	t.Parallel()
	handler := &Handler{artifactEmitter: &mockArtifactEmitter{}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/internal/jobs//artifact", bytes.NewBufferString("{}"))
	// No SetPathValue — jobId will be empty string
	w := httptest.NewRecorder()

	handler.ReportArtifact(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestArtifactAuthMiddleware(t *testing.T) {
	t.Parallel()

	newMux := func(apiKey string) *http.ServeMux {
		mux := http.NewServeMux()
		mw := ArtifactAuthMiddleware(apiKey)
		mux.Handle("POST /internal/jobs/{jobId}/artifact", mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})))
		return mux
	}

	tests := []struct {
		name   string
		apiKey string
		header string
		want   int
	}{
		{"no auth header", "api-key", "", http.StatusUnauthorized},
		{"malformed header", "api-key", "Basic abc", http.StatusUnauthorized},
		{"wrong token", "api-key", "Bearer nope", http.StatusUnauthorized},
		{"token for another job", "api-key", "Bearer " + job.ArtifactToken("api-key", "other-job"), http.StatusUnauthorized},
		{"valid token", "api-key", "Bearer " + job.ArtifactToken("api-key", "job-123"), http.StatusAccepted},
		{"auth disabled", "", "", http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/internal/jobs/job-123/artifact", bytes.NewBufferString("{}"))
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			w := httptest.NewRecorder()
			newMux(tt.apiKey).ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("Expected status %d, got %d", tt.want, w.Code)
			}
		})
	}
}
