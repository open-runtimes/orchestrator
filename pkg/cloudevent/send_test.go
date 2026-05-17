package cloudevent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPError_Error(t *testing.T) {
	t.Parallel()
	tests := []struct {
		statusCode int
		expected   string
	}{
		{400, "HTTP 400"},
		{404, "HTTP 404"},
		{500, "HTTP 500"},
		{503, "HTTP 503"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			err := &HTTPError{StatusCode: tt.statusCode}
			if err.Error() != tt.expected {
				t.Errorf("HTTPError{%d}.Error() = %q, want %q", tt.statusCode, err.Error(), tt.expected)
			}
		})
	}
}

func TestIsClientError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "400 Bad Request",
			err:      &HTTPError{StatusCode: 400},
			expected: true,
		},
		{
			name:     "401 Unauthorized",
			err:      &HTTPError{StatusCode: 401},
			expected: true,
		},
		{
			name:     "404 Not Found",
			err:      &HTTPError{StatusCode: 404},
			expected: true,
		},
		{
			name:     "499 client error boundary",
			err:      &HTTPError{StatusCode: 499},
			expected: true,
		},
		{
			name:     "500 Internal Server Error",
			err:      &HTTPError{StatusCode: 500},
			expected: false,
		},
		{
			name:     "503 Service Unavailable",
			err:      &HTTPError{StatusCode: 503},
			expected: false,
		},
		{
			name:     "399 not a client error",
			err:      &HTTPError{StatusCode: 399},
			expected: false,
		},
		{
			name:     "non-HTTP error",
			err:      context.DeadlineExceeded,
			expected: false,
		},
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsClientError(tt.err)
			if got != tt.expected {
				t.Errorf("IsClientError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

func TestGenerateSignature(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"test":"data"}`)
	key := "secret-key"

	signature := generateSignature(payload, key)

	// Verify it starts with sha256=
	if len(signature) < 7 || signature[:7] != "sha256=" {
		t.Errorf("signature should start with 'sha256=', got %q", signature)
	}

	// Verify the hex part is 64 characters (SHA256 = 32 bytes = 64 hex chars)
	hexPart := signature[7:]
	if len(hexPart) != 64 {
		t.Errorf("signature hex part should be 64 chars, got %d", len(hexPart))
	}

	// Verify deterministic output
	signature2 := generateSignature(payload, key)
	if signature != signature2 {
		t.Error("signature should be deterministic")
	}

	// Different key should produce different signature
	signature3 := generateSignature(payload, "different-key")
	if signature == signature3 {
		t.Error("different keys should produce different signatures")
	}
}

func TestSendProtocolHeadersOverrideCustomHeaders(t *testing.T) {
	t.Parallel()

	gotCh := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCh <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewSender(time.Second)
	event := New("test.event", "test", "job-1", "evt-1", nil)

	err := sender.Send(t.Context(), server.URL, event, SendOptions{
		Signature: "sha256=authoritative",
		Headers: map[string]string{
			"Content-Type":     "text/plain",
			"Ce-Type":          "wrong.type",
			"X-Signature-256":  "sha256=custom",
			"X-Custom-Header":  "custom",
			"Ce-Specversion":   "0.3",
			"Ce-Source":        "wrong",
			"Ce-Subject":       "wrong",
			"Ce-Id":            "wrong",
			"Ce-Time":          "wrong",
			"X-Another-Header": "another",
		},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	got := <-gotCh

	if got.Get("Content-Type") != "application/cloudevents+json" {
		t.Errorf("Content-Type = %q", got.Get("Content-Type"))
	}
	if got.Get("Ce-Type") != "test.event" {
		t.Errorf("Ce-Type = %q", got.Get("Ce-Type"))
	}
	if got.Get("X-Signature-256") != "sha256=authoritative" {
		t.Errorf("X-Signature-256 = %q", got.Get("X-Signature-256"))
	}
	if got.Get("X-Custom-Header") != "custom" {
		t.Errorf("X-Custom-Header = %q", got.Get("X-Custom-Header"))
	}
}
