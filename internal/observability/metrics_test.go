package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewMetrics(t *testing.T) {
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	metrics, err := NewMetrics(t.Context())
	if err != nil {
		t.Fatalf("Failed to create metrics: %v", err)
	}
	if metrics == nil {
		t.Fatal("Expected metrics to be non-nil")
	}
	if err := metrics.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNewMetricsPushesOTLPHTTPOnShutdown(t *testing.T) {
	request := make(chan []byte, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			t.Errorf("path: want /v1/metrics, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("content type: want application/x-protobuf, got %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		request <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	// An endpoint alone enables OTLP export; explicitly selecting "otlp" is
	// only needed when relying on the exporter's localhost default.
	t.Setenv("OTEL_METRICS_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", receiver.URL+"/v1/metrics")

	metrics, err := NewMetrics(t.Context())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	metrics.RecordHTTPRequest(t.Context(), http.MethodGet, "/readyz", http.StatusOK, 0.01)

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := metrics.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case body := <-request:
		if len(body) == 0 {
			t.Fatal("OTLP request body is empty")
		}
	default:
		t.Fatal("expected OTLP metrics request")
	}
}

func TestRecordHTTPRequest(t *testing.T) {
	metrics := newTestMetrics(t)
	ctx := t.Context()

	// Should not panic
	metrics.RecordHTTPRequest(ctx, "GET", "/health", 200, 0.001)
	metrics.RecordHTTPRequest(ctx, "POST", "/v1/jobs", 202, 0.050)
	metrics.RecordHTTPRequest(ctx, "GET", "/v1/jobs/abc123", 200, 0.010)
	metrics.RecordHTTPRequest(ctx, "GET", "/v1/jobs/xyz789", 404, 0.005)
	metrics.RecordHTTPRequest(ctx, "DELETE", "/v1/jobs/abc123", 204, 0.100)
	metrics.RecordHTTPRequest(ctx, "POST", "/v1/jobs", 500, 0.001)
}

func TestRecordJobMetrics(t *testing.T) {
	metrics := newTestMetrics(t)
	ctx := t.Context()

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
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics := &Metrics{meter: provider.Meter("orchestrator"), provider: provider}
	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	var active int64
	if err := metrics.ObserveInt64("jobs_active", "test gauge", func() int64 { return active }); err != nil {
		t.Fatalf("ObserveInt64: %v", err)
	}

	collect := func() int64 {
		t.Helper()
		var data metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &data); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		for _, scope := range data.ScopeMetrics {
			for _, metric := range scope.Metrics {
				if metric.Name != "jobs_active" {
					continue
				}
				gauge, ok := metric.Data.(metricdata.Gauge[int64])
				if !ok || len(gauge.DataPoints) != 1 {
					t.Fatalf("jobs_active: unexpected data %#v", metric.Data)
				}
				return gauge.DataPoints[0].Value
			}
		}
		t.Fatal("jobs_active metric is absent")
		return 0
	}

	active = 3
	if got := collect(); got != 3 {
		t.Errorf("jobs_active: want 3, got %d", got)
	}

	// Two jobs finish that this process never saw start — the deficit an
	// UpDownCounter would carry forever. The gauge just re-reads the truth.
	active = 1
	if got := collect(); got != 1 {
		t.Errorf("jobs_active after unmatched completions: want 1, got %d", got)
	}
}

func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	metrics, err := NewMetrics(t.Context())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })
	return metrics
}
