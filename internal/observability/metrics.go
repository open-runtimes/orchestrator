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

	// Leadership (K8s backend; zero everywhere else). Gauge is 1 on the leader
	// replica and 0 (or absent) on followers, labelled with the identity so
	// operators can see who's holding the lease at a glance.
	LeaderGauge            metric.Int64Gauge
	LeaderTransitionsTotal metric.Int64Counter

	// Status cache effectiveness (K8s backend).
	StatusCacheHits   metric.Int64Counter
	StatusCacheMisses metric.Int64Counter

	// Tracker saturation (K8s backend): number of in-flight per-job trackers
	// the leader currently owns. The practical concurrent-jobs ceiling.
	Trackers metric.Int64UpDownCounter

	// K8s API cost: every Run/Stop/Status/List and every informer list+watch
	// goes through the apiserver. When latency rises here, our HTTP latency
	// rises with it — surface the cause.
	K8sAPIDuration metric.Float64Histogram
	K8sAPIErrors   metric.Int64Counter
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

	// Leadership (K8s backend).
	m.LeaderGauge, err = meter.Int64Gauge(
		"orchestrator_leader",
		metric.WithDescription("1 on the replica currently holding the leader lease, 0 otherwise"),
	)
	if err != nil {
		return nil, nil, err
	}
	m.LeaderTransitionsTotal, err = meter.Int64Counter(
		"orchestrator_leader_transitions_total",
		metric.WithDescription("Total leader acquisitions observed by this replica"),
	)
	if err != nil {
		return nil, nil, err
	}

	// Status cache.
	m.StatusCacheHits, err = meter.Int64Counter(
		"orchestrator_status_cache_hits_total",
		metric.WithDescription("Total Status calls served from the TTL cache"),
	)
	if err != nil {
		return nil, nil, err
	}
	m.StatusCacheMisses, err = meter.Int64Counter(
		"orchestrator_status_cache_misses_total",
		metric.WithDescription("Total Status calls that missed the cache and hit the K8s API"),
	)
	if err != nil {
		return nil, nil, err
	}

	// Tracker saturation.
	m.Trackers, err = meter.Int64UpDownCounter(
		"orchestrator_trackers",
		metric.WithDescription("In-flight per-job lifecycle trackers on the leader (saturation)"),
	)
	if err != nil {
		return nil, nil, err
	}

	// K8s API.
	m.K8sAPIDuration, err = meter.Float64Histogram(
		"k8s_api_request_duration_seconds",
		metric.WithDescription("K8s API request latency in seconds, by verb and resource"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return nil, nil, err
	}
	m.K8sAPIErrors, err = meter.Int64Counter(
		"k8s_api_errors_total",
		metric.WithDescription("Total K8s API responses with status >= 400 (or transport errors)"),
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

// RecordLeadership sets this replica's leader gauge and, when acquired, bumps
// the transitions counter. identity labels both metrics.
func (m *Metrics) RecordLeadership(ctx context.Context, identity string, acquired bool) {
	attrs := metric.WithAttributes(identityAttr(identity))
	if acquired {
		m.LeaderGauge.Record(ctx, 1, attrs)
		m.LeaderTransitionsTotal.Add(ctx, 1, attrs)
	} else {
		m.LeaderGauge.Record(ctx, 0, attrs)
	}
}

// RecordStatusCacheHit bumps the Status cache-hit counter.
func (m *Metrics) RecordStatusCacheHit(ctx context.Context) {
	m.StatusCacheHits.Add(ctx, 1)
}

// RecordStatusCacheMiss bumps the Status cache-miss counter.
func (m *Metrics) RecordStatusCacheMiss(ctx context.Context) {
	m.StatusCacheMisses.Add(ctx, 1)
}

// RecordTrackerDelta adjusts the in-flight tracker gauge by delta (+1 / -1).
func (m *Metrics) RecordTrackerDelta(ctx context.Context, delta int64) {
	m.Trackers.Add(ctx, delta)
}

// RecordK8sAPIRequest records a K8s API call's latency and, if it returned a
// 4xx/5xx or failed in transport, increments the error counter.
func (m *Metrics) RecordK8sAPIRequest(ctx context.Context, verb, resource string, durationSeconds float64, status int) {
	attrs := metric.WithAttributes(verbAttr(verb), resourceAttr(resource))
	m.K8sAPIDuration.Record(ctx, durationSeconds, attrs)
	if status < 0 || status >= 400 {
		errAttrs := metric.WithAttributes(verbAttr(verb), resourceAttr(resource), statusAttr(status))
		m.K8sAPIErrors.Add(ctx, 1, errAttrs)
	}
}
