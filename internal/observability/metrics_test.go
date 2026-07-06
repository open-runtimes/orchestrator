package observability

import (
	"testing"
)

func TestNewMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	metrics, handler, err := NewMetrics(ctx)
	if err != nil {
		t.Fatalf("Failed to create metrics: %v", err)
	}

	if metrics == nil {
		t.Fatal("Expected metrics to be non-nil")
	}

	if handler == nil {
		t.Fatal("Expected handler to be non-nil")
	}
}

func TestRecordHTTPRequest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	metrics, _, err := NewMetrics(ctx)
	if err != nil {
		t.Fatalf("Failed to create metrics: %v", err)
	}

	// Should not panic
	metrics.RecordHTTPRequest(ctx, "GET", "/health", 200, 0.001)
	metrics.RecordHTTPRequest(ctx, "POST", "/v1/jobs", 202, 0.050)
	metrics.RecordHTTPRequest(ctx, "GET", "/v1/jobs/abc123", 200, 0.010)
	metrics.RecordHTTPRequest(ctx, "GET", "/v1/jobs/xyz789", 404, 0.005)
	metrics.RecordHTTPRequest(ctx, "DELETE", "/v1/jobs/abc123", 204, 0.100)
	metrics.RecordHTTPRequest(ctx, "POST", "/v1/jobs", 500, 0.001)
}

func TestRecordJobMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	metrics, _, err := NewMetrics(ctx)
	if err != nil {
		t.Fatalf("Failed to create metrics: %v", err)
	}

	// Should not panic
	metrics.RecordJobCreated(ctx, "alpine:latest")
	metrics.RecordJobCreated(ctx, "python:3.11")
	metrics.RecordJobCompleted(ctx, "alpine:latest", true, 5.5)
	metrics.RecordJobCompleted(ctx, "python:3.11", false, 120.0)
	metrics.RecordJobCancelled(ctx, "alpine:latest")
}

func TestNormalizePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"/health", "/health"},
		{"/metrics", "/metrics"},
		{"/v1/jobs", "/v1/jobs"},
		{"/v1/jobs/abc123", "/v1/jobs/{id}"},
		{"/v1/jobs/xyz-789-def", "/v1/jobs/{id}"},
		{"/v1/deployments", "/v1/deployments"},
		{"/v1/deployments/web", "/v1/deployments/{id}"},
		{"/v1/deployments/web/traffic", "/v1/deployments/{id}/traffic"},
		{"/v1/deployments/web/revisions", "/v1/deployments/{id}/revisions"},
		{"/v1/deployment-pools", "/v1/deployment-pools"},
		{"/v1/deployment-pools/py", "/v1/deployment-pools/{id}"},
		{"/v1/deployment-pools/py/activations", "/v1/deployment-pools/{id}/activations"},
		{"/v1/deployment-pools/py/activations/run-42", "/v1/deployment-pools/{id}/activations/{actId}"},
		{"/other/path", "/other/path"},
	}

	for _, tt := range tests {
		result := normalizePath(tt.input)
		if result != tt.expected {
			t.Errorf("normalizePath(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
