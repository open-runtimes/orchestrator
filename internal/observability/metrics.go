package observability

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds all application metrics implementing the golden 4 signals:
// - Latency: How long requests/jobs take
// - Traffic: Request/job throughput
// - Errors: Rate of failures
// - Saturation: Resource utilization (concurrent jobs/requests)
type Metrics struct {
	meter    metric.Meter
	provider *sdkmetric.MeterProvider

	// HTTP metrics (Latency, Traffic, Errors)
	HTTPRequestDuration metric.Float64Histogram
	HTTPRequestsTotal   metric.Int64Counter
	HTTPErrorsTotal     metric.Int64Counter

	// Job metrics (Latency, Traffic, Errors). JobDuration is labelled
	// image+success, so its _count doubles as the completion counter and the
	// error counter — no separate instrument to keep in step with it.
	// Saturation (jobs_active) is an async gauge, registered by the wiring.
	JobDuration metric.Float64Histogram
	JobsTotal   metric.Int64Counter

	// Dispatcher metrics (Latency, Traffic, Errors). Queue depth is an async
	// gauge, registered by the wiring.
	DispatcherDuration  metric.Float64Histogram
	DispatcherDelivered metric.Int64Counter
	DispatcherFailed    metric.Int64Counter
	DispatcherDropped   metric.Int64Counter
	DispatcherRequeued  metric.Int64Counter

	// Leadership (K8s backend; zero everywhere else). Gauge is 1 on the leader
	// replica and 0 (or absent) on followers, labelled with the identity so
	// operators can see who's holding the lease at a glance.
	LeaderGauge            metric.Int64Gauge
	LeaderTransitionsTotal metric.Int64Counter

	// Status cache effectiveness (K8s backend).
	StatusCacheHits   metric.Int64Counter
	StatusCacheMisses metric.Int64Counter

	// K8s API cost: every Run/Stop/Status/List and every informer list+watch
	// goes through the apiserver. When latency rises here, our HTTP latency
	// rises with it — surface the cause.
	K8sAPIDuration metric.Float64Histogram
	K8sAPIErrors   metric.Int64Counter

	// Deployment metrics (Latency, Traffic, Saturation). Rollout duration is
	// revision-minted → traffic-cut, observed by the leader's reconciler.
	DeploymentsApplied        metric.Int64Counter
	DeploymentsActive         metric.Int64Gauge
	RolloutDuration           metric.Float64Histogram
	RolloutCuts               metric.Int64Counter
	RevisionReconcileDuration metric.Float64Histogram
	RevisionReconcileErrors   metric.Int64Counter
	RevisionQueueWait         metric.Float64Histogram
	RevisionPodCreateDuration metric.Float64Histogram
	RevisionPodCreates        metric.Int64Counter
	RevisionPodDeletes        metric.Int64Counter
	RevisionLeaderConvergence metric.Float64Histogram

	// Activator metrics: the cold/async edge. Hold duration is the time a
	// request waits for serving capacity (the client-visible cold-start cost);
	// queued is the autoscaler's hold-up signal as a gauge.
	ActivatorHoldDuration metric.Float64Histogram
	ActivatorQueued       metric.Int64UpDownCounter
	ActivatorRaises       metric.Int64Counter
	ActivatorAsync        metric.Int64Counter

	// Autoscaler metrics: what the loop decided and whether its inputs are
	// healthy. Desired is labelled per deployment (bounded by deployment
	// count, like jobs' image label).
	AutoscalerDesired      metric.Int64Gauge
	AutoscalerScales       metric.Int64Counter
	AutoscalerScrapeErrors metric.Int64Counter

	// Pool metrics (Latency, Traffic, Errors, Saturation). Warm/claimed are
	// recorded by the leader's control loop; claim conflicts are the racing
	// losers (healthy at low rates), poisoned pods are failed claims.
	PoolClaims              metric.Int64Counter
	PoolClaimsActive        metric.Int64UpDownCounter
	PoolClaimDuration       metric.Float64Histogram
	PoolReservationDuration metric.Float64Histogram
	PoolClaimConflicts      metric.Int64Counter
	PoolPoisoned            metric.Int64Counter
	PoolBurst               metric.Int64Counter
	PoolWarm                metric.Int64Gauge
	PoolClaimed             metric.Int64Gauge
}

// instruments builds meters while accumulating the first error, sparing
// NewMetrics an if-err block per instrument.
type instruments struct {
	meter metric.Meter
	err   error
}

func (b *instruments) counter(name, desc string) metric.Int64Counter {
	c, err := b.meter.Int64Counter(name, metric.WithDescription(desc))
	if b.err == nil {
		b.err = err
	}
	return c
}

func (b *instruments) upDown(name, desc string) metric.Int64UpDownCounter {
	c, err := b.meter.Int64UpDownCounter(name, metric.WithDescription(desc))
	if b.err == nil {
		b.err = err
	}
	return c
}

func (b *instruments) gauge(name, desc string) metric.Int64Gauge {
	g, err := b.meter.Int64Gauge(name, metric.WithDescription(desc))
	if b.err == nil {
		b.err = err
	}
	return g
}

func (b *instruments) histogram(name, desc string, buckets ...float64) metric.Float64Histogram {
	h, err := b.meter.Float64Histogram(name,
		metric.WithDescription(desc),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(buckets...),
	)
	if b.err == nil {
		b.err = err
	}
	return h
}

// NewMetrics creates all metrics and configures periodic OTLP push export from
// the standard OpenTelemetry environment variables. Export is disabled when
// neither OTEL_METRICS_EXPORTER nor an OTLP endpoint is configured.
func NewMetrics(ctx context.Context) (*Metrics, error) {
	reader, err := newMetricReader(ctx)
	if err != nil {
		return nil, err
	}

	providerOpts := make([]sdkmetric.Option, 0, 1)
	if reader != nil {
		providerOpts = append(providerOpts, sdkmetric.WithReader(reader))
	}
	provider := sdkmetric.NewMeterProvider(providerOpts...)
	otel.SetMeterProvider(provider)

	b := &instruments{meter: provider.Meter("orchestrator")}
	m := &Metrics{meter: b.meter, provider: provider}

	// HTTP metrics
	m.HTTPRequestDuration = b.histogram("http_request_duration_seconds",
		"HTTP request latency in seconds",
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)
	m.HTTPRequestsTotal = b.counter("http_requests_total", "Total number of HTTP requests")
	m.HTTPErrorsTotal = b.counter("http_errors_total", "Total number of HTTP errors (4xx and 5xx)")

	// Job metrics
	m.JobDuration = b.histogram("job_duration_seconds",
		"Job execution duration in seconds",
		1, 5, 10, 30, 60, 120, 300, 600, 900, 1800)
	m.JobsTotal = b.counter("jobs_total", "Total number of jobs created")

	// Dispatcher metrics
	m.DispatcherDuration = b.histogram("dispatcher_duration_seconds",
		"Callback delivery latency in seconds",
		0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)
	m.DispatcherDelivered = b.counter("dispatcher_delivered_total", "Total events successfully delivered")
	m.DispatcherFailed = b.counter("dispatcher_failed_total", "Total events failed after retries")
	m.DispatcherDropped = b.counter("dispatcher_dropped_total", "Total events dropped (buffer full or max requeues)")
	m.DispatcherRequeued = b.counter("dispatcher_requeued_total", "Total events requeued due to open circuit")

	// Leadership (K8s backend).
	m.LeaderGauge = b.gauge("orchestrator_leader", "1 on the replica currently holding the leader lease, 0 otherwise")
	m.LeaderTransitionsTotal = b.counter("orchestrator_leader_transitions_total", "Total leader acquisitions observed by this replica")

	// Status cache.
	m.StatusCacheHits = b.counter("orchestrator_status_cache_hits_total", "Total Status calls served from the TTL cache")
	m.StatusCacheMisses = b.counter("orchestrator_status_cache_misses_total", "Total Status calls that missed the cache and hit the K8s API")

	// K8s API.
	m.K8sAPIDuration = b.histogram("k8s_api_request_duration_seconds",
		"K8s API request latency in seconds, by verb and resource",
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)
	m.K8sAPIErrors = b.counter("k8s_api_errors_total", "Total K8s API responses with status >= 400 (or transport errors)")

	// Deployments.
	m.DeploymentsApplied = b.counter("deployments_applied_total",
		"Total deployment applies, labelled created=true|false (create vs update)")
	m.DeploymentsActive = b.gauge("deployments_active",
		"Number of managed deployments (K8s backend; recorded by the leader's reconciler)")
	m.RolloutDuration = b.histogram("deployment_rollout_duration_seconds",
		"Time from a revision being minted to traffic auto-cutting to it",
		1, 2.5, 5, 10, 30, 60, 120, 300, 600)
	m.RolloutCuts = b.counter("deployment_rollout_cuts_total", "Total traffic auto-cuts to a newly ready revision")
	m.RevisionReconcileDuration = b.histogram("revision_reconcile_duration_seconds",
		"Direct-Pod Revision reconciliation duration",
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5)
	m.RevisionReconcileErrors = b.counter("revision_reconcile_errors_total", "Failed Revision reconciliations")
	m.RevisionQueueWait = b.histogram("revision_queue_wait_seconds",
		"Time a Revision waited in the controller queue",
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30)
	m.RevisionPodCreateDuration = b.histogram("revision_desired_to_pod_created_seconds",
		"Time from a Revision event reaching the controller to a successful Pod create",
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)
	m.RevisionPodCreates = b.counter("revision_pod_creates_total", "Pods created directly by the Revision controller")
	m.RevisionPodDeletes = b.counter("revision_pod_deletes_total", "Pods deleted by the Revision controller, by reason")
	m.RevisionLeaderConvergence = b.histogram("revision_leader_convergence_seconds",
		"Time from leader worker start until the initial cached Revision inventory converges",
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60)

	// Activator.
	m.ActivatorHoldDuration = b.histogram("activator_hold_duration_seconds",
		"Time a request waited for serving capacity, labelled outcome=served|timeout",
		0.001, 0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300)
	m.ActivatorQueued = b.upDown("activator_queued", "Requests currently held waiting for capacity (saturation)")
	m.ActivatorRaises = b.counter("activator_raises_total", "Total cold scale-ups requested while holding traffic")
	m.ActivatorAsync = b.counter("activator_async_total", "Total async requests, labelled result=delivered|failed")

	// Autoscaler.
	m.AutoscalerDesired = b.gauge("autoscaler_desired_replicas", "Replicas the autoscaler wants, per deployment")
	m.AutoscalerScales = b.counter("autoscaler_scale_events_total", "Total scale writes, labelled direction=up|down")
	m.AutoscalerScrapeErrors = b.counter("autoscaler_scrape_errors_total", "Total failures scraping concurrency/queue sources")

	// Pools.
	m.PoolClaims = b.counter("pool_claims_total", "Total workload claims, per pool")
	m.PoolClaimsActive = b.upDown("pool_claims_active", "Claims currently in flight, per pool (saturation)")
	m.PoolClaimDuration = b.histogram("pool_claim_duration_seconds",
		"Claim wall time through serving, per pool and success",
		0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 600, 1800)
	m.PoolReservationDuration = b.histogram("pool_claim_reservation_duration_seconds",
		"Kubernetes metadata reservation latency, per pool and success",
		0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1)
	m.PoolClaimConflicts = b.counter("pool_claim_conflicts_total", "Total atomic reservation or sidecar claim races lost")
	m.PoolPoisoned = b.counter("pool_poisoned_total", "Total pods poisoned by failed artifact materialization")
	m.PoolBurst = b.counter("pool_burst_total", "Total claims arriving at an empty pool, labelled policy=reject|cold")
	m.PoolWarm = b.gauge("pool_warm", "Unclaimed warm-ready pods, per pool")
	m.PoolClaimed = b.gauge("pool_claimed", "Claimed workload pods, per pool")

	if b.err != nil {
		_ = provider.Shutdown(ctx)
		return nil, b.err
	}
	return m, nil
}

func newMetricReader(ctx context.Context) (sdkmetric.Reader, error) {
	exporterName := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_METRICS_EXPORTER")))
	if exporterName == "" {
		if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
			return nil, nil
		}
		exporterName = "otlp"
	}

	switch exporterName {
	case "none":
		return nil, nil
	case "otlp":
	default:
		return nil, fmt.Errorf("unsupported OTEL_METRICS_EXPORTER %q (expected otlp or none)", exporterName)
	}

	protocol := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")))
	if protocol == "" {
		protocol = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
	}
	if protocol == "" {
		protocol = "http/protobuf"
	}

	var exporter sdkmetric.Exporter
	var err error
	switch protocol {
	case "http/protobuf":
		exporter, err = otlpmetrichttp.New(ctx)
	case "grpc":
		exporter, err = otlpmetricgrpc.New(ctx)
	default:
		return nil, fmt.Errorf("unsupported OTLP metrics protocol %q (expected http/protobuf or grpc)", protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("create OTLP metrics exporter: %w", err)
	}
	return sdkmetric.NewPeriodicReader(exporter), nil
}

// Shutdown flushes pending metrics and stops the periodic OTLP exporter.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if m == nil || m.provider == nil {
		return nil
	}
	return m.provider.Shutdown(ctx)
}

// ObserveInt64 registers an asynchronous gauge that reads observe at collection
// time. Prefer it over an UpDownCounter for anything we can just read: a
// synchronous +1/-1 pair only stays balanced while one process sees both halves,
// and ours don't — a restart resets the counter while the jobs it counted keep
// running, a K8s leadership handover moves the -1 to a replica that never did
// the +1, and either way the gauge drifts negative and never recovers. An async
// gauge re-derives the truth on every collection, so it cannot drift.
//
// Safe to call on a nil *Metrics (metrics disabled).
func (m *Metrics) ObserveInt64(name, desc string, observe func() int64) error {
	if m == nil {
		return nil
	}
	_, err := m.meter.Int64ObservableGauge(name,
		metric.WithDescription(desc),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(observe())
			return nil
		}),
	)
	return err
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
	m.JobsTotal.Add(ctx, 1, metric.WithAttributes(imageAttr(image)))
}

// RecordJobCompleted records a job completing (success or failure). The
// histogram's _count series, split by success, is the completion and error
// rate — job_duration_seconds_count{success="false"} needs no counter of its own.
func (m *Metrics) RecordJobCompleted(ctx context.Context, image string, success bool, durationSeconds float64) {
	m.JobDuration.Record(ctx, durationSeconds,
		metric.WithAttributes(imageAttr(image), successAttr(success)))
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

// RecordDeploymentApplied records a deployment apply (create or update).
func (m *Metrics) RecordDeploymentApplied(ctx context.Context, created bool) {
	m.DeploymentsApplied.Add(ctx, 1, metric.WithAttributes(createdAttr(created)))
}

// RecordDeploymentsActive records the managed-deployment count.
func (m *Metrics) RecordDeploymentsActive(ctx context.Context, count int64) {
	m.DeploymentsActive.Record(ctx, count)
}

// RecordRolloutCut records traffic auto-cutting to a newly ready revision,
// with the revision's minted→ready duration.
func (m *Metrics) RecordRolloutCut(ctx context.Context, durationSeconds float64) {
	m.RolloutCuts.Add(ctx, 1)
	m.RolloutDuration.Record(ctx, durationSeconds)
}

func (m *Metrics) RecordRevisionReconcile(ctx context.Context, success bool, durationSeconds float64) {
	m.RevisionReconcileDuration.Record(ctx, durationSeconds, metric.WithAttributes(successAttr(success)))
	if !success {
		m.RevisionReconcileErrors.Add(ctx, 1)
	}
}

func (m *Metrics) RecordRevisionQueueWait(ctx context.Context, durationSeconds float64) {
	m.RevisionQueueWait.Record(ctx, durationSeconds)
}

func (m *Metrics) RecordRevisionPodCreate(ctx context.Context, durationSeconds float64) {
	m.RevisionPodCreates.Add(ctx, 1)
	m.RevisionPodCreateDuration.Record(ctx, durationSeconds)
}

func (m *Metrics) RecordRevisionPodDelete(ctx context.Context, reason string) {
	m.RevisionPodDeletes.Add(ctx, 1, metric.WithAttributes(reasonAttr(reason)))
}

func (m *Metrics) RecordRevisionLeaderConvergence(ctx context.Context, durationSeconds float64) {
	m.RevisionLeaderConvergence.Record(ctx, durationSeconds)
}

// RecordActivatorHold records a completed capacity hold.
func (m *Metrics) RecordActivatorHold(ctx context.Context, component, outcome string, durationSeconds float64) {
	m.ActivatorHoldDuration.Record(ctx, durationSeconds, metric.WithAttributes(componentAttr(component), outcomeAttr(outcome)))
}

// RecordActivatorQueueDelta adjusts the held-request gauge (+1 / -1).
func (m *Metrics) RecordActivatorQueueDelta(ctx context.Context, component string, delta int64) {
	m.ActivatorQueued.Add(ctx, delta, metric.WithAttributes(componentAttr(component)))
}

// RecordActivatorRaise records a cold scale-up request.
func (m *Metrics) RecordActivatorRaise(ctx context.Context, component string) {
	m.ActivatorRaises.Add(ctx, 1, metric.WithAttributes(componentAttr(component)))
}

// RecordActivatorAsync records an async request's final result.
func (m *Metrics) RecordActivatorAsync(ctx context.Context, component, result string) {
	m.ActivatorAsync.Add(ctx, 1, metric.WithAttributes(componentAttr(component), resultAttr(result)))
}

// RecordAutoscalerDesired records the autoscaler's decision for a deployment.
func (m *Metrics) RecordAutoscalerDesired(ctx context.Context, id string, replicas int64) {
	m.AutoscalerDesired.Record(ctx, replicas, metric.WithAttributes(deploymentAttr(id)))
}

// RecordAutoscalerScale records a scale write.
func (m *Metrics) RecordAutoscalerScale(ctx context.Context, direction string) {
	m.AutoscalerScales.Add(ctx, 1, metric.WithAttributes(directionAttr(direction)))
}

// RecordAutoscalerScrapeError records a failed metrics scrape.
func (m *Metrics) RecordAutoscalerScrapeError(ctx context.Context) {
	m.AutoscalerScrapeErrors.Add(ctx, 1)
}

// RecordPoolClaimStarted records a claim entering flight.
func (m *Metrics) RecordPoolClaimStarted(ctx context.Context, kind, id string) {
	attrs := metric.WithAttributes(kindAttr(kind), poolAttr(id))
	m.PoolClaims.Add(ctx, 1, attrs)
	m.PoolClaimsActive.Add(ctx, 1, attrs)
}

// RecordPoolClaimFinished records a claim leaving flight with its
// wall time (claim through serving).
func (m *Metrics) RecordPoolClaimFinished(ctx context.Context, kind, id string, success bool, durationSeconds float64) {
	m.PoolClaimsActive.Add(ctx, -1, metric.WithAttributes(kindAttr(kind), poolAttr(id)))
	m.PoolClaimDuration.Record(ctx, durationSeconds, metric.WithAttributes(kindAttr(kind), poolAttr(id), successAttr(success)))
}

// RecordPoolReservation records the API-server write that serializes a warm
// claim and stamps its final workload identity before activation.
func (m *Metrics) RecordPoolReservation(ctx context.Context, kind, id string, success bool, durationSeconds float64) {
	m.PoolReservationDuration.Record(ctx, durationSeconds,
		metric.WithAttributes(kindAttr(kind), poolAttr(id), successAttr(success)))
}

// RecordPoolConflict records a lost claim race.
func (m *Metrics) RecordPoolConflict(ctx context.Context, kind, id string) {
	m.PoolClaimConflicts.Add(ctx, 1, metric.WithAttributes(kindAttr(kind), poolAttr(id)))
}

// RecordPoolPoisoned records a pod poisoned by a failed activation.
func (m *Metrics) RecordPoolPoisoned(ctx context.Context, kind, id string) {
	m.PoolPoisoned.Add(ctx, 1, metric.WithAttributes(kindAttr(kind), poolAttr(id)))
}

// RecordPoolBurst records an activation arriving at an empty pool and the
// policy that decided its fate.
func (m *Metrics) RecordPoolBurst(ctx context.Context, kind, id, policy string) {
	m.PoolBurst.Add(ctx, 1, metric.WithAttributes(kindAttr(kind), poolAttr(id), policyAttr(policy)))
}

// RecordPoolCapacity records a pool's warm/claimed pod counts.
func (m *Metrics) RecordPoolCapacity(ctx context.Context, kind, id string, warm, claimed int64) {
	attrs := metric.WithAttributes(kindAttr(kind), poolAttr(id))
	m.PoolWarm.Record(ctx, warm, attrs)
	m.PoolClaimed.Record(ctx, claimed, attrs)
}
