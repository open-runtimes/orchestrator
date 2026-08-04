package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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
}

// Saturation gauges must be read at collection time, not tallied: a job that
// outlives a restart (or whose exit is observed by a different replica) reports
// a completion the process never counted a start for, and a synchronous
// UpDownCounter would go permanently negative on it.
func TestObserveInt64_ReadsLiveValueAtCollection(t *testing.T) {
	t.Parallel()

	// A private registry, so the scrape is unaffected by the other tests in
	// this package registering the same instrument names in the default one.
	registry := promclient.NewRegistry()
	exporter, err := prometheus.New(prometheus.WithRegisterer(registry))
	if err != nil {
		t.Fatalf("exporter: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	metrics := &Metrics{meter: provider.Meter("orchestrator")}
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})

	var active int64
	if err := metrics.ObserveInt64("jobs_active", "test gauge", func() int64 { return active }); err != nil {
		t.Fatalf("ObserveInt64: %v", err)
	}

	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
		for line := range strings.SplitSeq(rec.Body.String(), "\n") {
			if strings.HasPrefix(line, "jobs_active{") {
				return line[strings.LastIndex(line, " ")+1:]
			}
		}
		return "<absent>"
	}

	active = 3
	if got := scrape(); got != "3" {
		t.Errorf("jobs_active: want 3, got %s", got)
	}

	// Two jobs finish that this process never saw start — the deficit an
	// UpDownCounter would carry forever. The gauge just re-reads the truth.
	active = 1
	if got := scrape(); got != "1" {
		t.Errorf("jobs_active after unmatched completions: want 1, got %s", got)
	}
}
