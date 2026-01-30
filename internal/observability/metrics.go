package observability

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds all application metrics implementing the golden 4 signals:
// - Latency: How long requests/jobs take
// - Traffic: Request/job throughput
// - Errors: Rate of failures
// - Saturation: Resource utilization (concurrent jobs/requests)
type Metrics struct {
	meter metric.Meter

	// HTTP metrics (Latency, Traffic, Errors)
	HTTPRequestDuration metric.Float64Histogram
	HTTPRequestsTotal   metric.Int64Counter
	HTTPErrorsTotal     metric.Int64Counter

	// Job metrics (Latency, Traffic, Errors, Saturation)
	JobDuration    metric.Float64Histogram
	JobsTotal      metric.Int64Counter
	JobErrorsTotal metric.Int64Counter
	JobsActive     metric.Int64UpDownCounter

	// Dispatcher metrics (Latency, Traffic, Errors, Saturation)
	DispatcherDuration   metric.Float64Histogram
	DispatcherDelivered  metric.Int64Counter
	DispatcherFailed     metric.Int64Counter
	DispatcherDropped    metric.Int64Counter
	DispatcherRequeued   metric.Int64Counter
	DispatcherQueueSize  metric.Int64Gauge
	DispatcherBufferSize int64 // config value for saturation calculation
}

// NewMetrics creates and registers all metrics with a Prometheus exporter.
func NewMetrics(ctx context.Context) (*Metrics, http.Handler, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, nil, err
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)

	meter := provider.Meter("orchestrator")
	m := &Metrics{meter: meter}

	// HTTP metrics
	m.HTTPRequestDuration, err = meter.Float64Histogram(
		"http_request_duration_seconds",
		metric.WithDescription("HTTP request latency in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return nil, nil, err
	}

	m.HTTPRequestsTotal, err = meter.Int64Counter(
		"http_requests_total",
		metric.WithDescription("Total number of HTTP requests"),
	)
	if err != nil {
		return nil, nil, err
	}

	m.HTTPErrorsTotal, err = meter.Int64Counter(
		"http_errors_total",
		metric.WithDescription("Total number of HTTP errors (4xx and 5xx)"),
	)
	if err != nil {
		return nil, nil, err
	}

	// Job metrics
	m.JobDuration, err = meter.Float64Histogram(
		"job_duration_seconds",
		metric.WithDescription("Job execution duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120, 300, 600, 900, 1800),
	)
	if err != nil {
		return nil, nil, err
	}

	m.JobsTotal, err = meter.Int64Counter(
		"jobs_total",
		metric.WithDescription("Total number of jobs created"),
	)
	if err != nil {
		return nil, nil, err
	}

	m.JobErrorsTotal, err = meter.Int64Counter(
		"job_errors_total",
		metric.WithDescription("Total number of failed jobs"),
	)
	if err != nil {
		return nil, nil, err
	}

	m.JobsActive, err = meter.Int64UpDownCounter(
		"jobs_active",
		metric.WithDescription("Number of currently running jobs (saturation)"),
	)
	if err != nil {
		return nil, nil, err
	}

	// Dispatcher metrics
	m.DispatcherDuration, err = meter.Float64Histogram(
		"dispatcher_duration_seconds",
		metric.WithDescription("Callback delivery latency in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return nil, nil, err
	}

	m.DispatcherDelivered, err = meter.Int64Counter(
		"dispatcher_delivered_total",
		metric.WithDescription("Total events successfully delivered"),
	)
	if err != nil {
		return nil, nil, err
	}

	m.DispatcherFailed, err = meter.Int64Counter(
		"dispatcher_failed_total",
		metric.WithDescription("Total events failed after retries"),
	)
	if err != nil {
		return nil, nil, err
	}

	m.DispatcherDropped, err = meter.Int64Counter(
		"dispatcher_dropped_total",
		metric.WithDescription("Total events dropped (buffer full or max requeues)"),
	)
	if err != nil {
		return nil, nil, err
	}

	m.DispatcherRequeued, err = meter.Int64Counter(
		"dispatcher_requeued_total",
		metric.WithDescription("Total events requeued due to open circuit"),
	)
	if err != nil {
		return nil, nil, err
	}

	m.DispatcherQueueSize, err = meter.Int64Gauge(
		"dispatcher_queue_size",
		metric.WithDescription("Current number of events in dispatcher queue (saturation)"),
	)
	if err != nil {
		return nil, nil, err
	}

	return m, promhttp.Handler(), nil
}

// RecordHTTPRequest records HTTP request metrics.
func (m *Metrics) RecordHTTPRequest(ctx context.Context, method, path string, statusCode int, durationSeconds float64) {
	attrs := metric.WithAttributes(
		methodAttr(method),
		pathAttr(path),
		statusAttr(statusCode),
	)

	m.HTTPRequestDuration.Record(ctx, durationSeconds, attrs)
	m.HTTPRequestsTotal.Add(ctx, 1, attrs)

	if statusCode >= 400 {
		m.HTTPErrorsTotal.Add(ctx, 1, attrs)
	}
}

// RecordJobCreated records a new job being created.
func (m *Metrics) RecordJobCreated(ctx context.Context, image string) {
	attrs := metric.WithAttributes(imageAttr(image))
	m.JobsTotal.Add(ctx, 1, attrs)
	m.JobsActive.Add(ctx, 1, attrs)
}

// RecordJobCompleted records a job completing (success or failure).
func (m *Metrics) RecordJobCompleted(ctx context.Context, image string, success bool, durationSeconds float64) {
	attrs := metric.WithAttributes(imageAttr(image), successAttr(success))
	m.JobDuration.Record(ctx, durationSeconds, attrs)
	m.JobsActive.Add(ctx, -1, metric.WithAttributes(imageAttr(image)))

	if !success {
		m.JobErrorsTotal.Add(ctx, 1, attrs)
	}
}

// RecordJobCancelled records a job being cancelled.
func (m *Metrics) RecordJobCancelled(ctx context.Context, image string) {
	attrs := metric.WithAttributes(imageAttr(image))
	m.JobsActive.Add(ctx, -1, attrs)
}

// RecordDispatcherDelivered records a successful event delivery with its duration.
func (m *Metrics) RecordDispatcherDelivered(ctx context.Context, durationSeconds float64) {
	m.DispatcherDelivered.Add(ctx, 1)
	m.DispatcherDuration.Record(ctx, durationSeconds)
}

// RecordDispatcherFailed records a failed event delivery.
func (m *Metrics) RecordDispatcherFailed(ctx context.Context) {
	m.DispatcherFailed.Add(ctx, 1)
}

// RecordDispatcherDropped records a dropped event.
func (m *Metrics) RecordDispatcherDropped(ctx context.Context) {
	m.DispatcherDropped.Add(ctx, 1)
}

// RecordDispatcherRequeued records a requeued event.
func (m *Metrics) RecordDispatcherRequeued(ctx context.Context) {
	m.DispatcherRequeued.Add(ctx, 1)
}

// RecordDispatcherQueueSize records the current queue size.
func (m *Metrics) RecordDispatcherQueueSize(ctx context.Context, size int64) {
	m.DispatcherQueueSize.Record(ctx, size)
}
