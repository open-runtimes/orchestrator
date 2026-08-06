# Observability

## Quick Start

```bash
docker compose up -d   # Start Prometheus + Grafana
task dev               # Start service with hot reload
```

- **Grafana**: http://localhost:3000 (no login required)
- **Prometheus**: http://localhost:9091
- **Metrics endpoint**: http://localhost:9090/metrics

## Endpoints

- `localhost:8080` - API server
- `localhost:9090/metrics` - Prometheus metrics
- `localhost:8080/livez` - Liveness (process running)
- `localhost:8080/readyz` - Readiness (backend reachable — Docker daemon or K8s API server)

## Design Decisions

### Separate Metrics Port

Metrics are served on port 9090, separate from the API on 8080.

**Why?** Allows different access controls. Metrics endpoints can be internal-only while API is exposed. Also prevents metrics scraping from affecting API latency measurements.

See: `cmd/jobs-service/main.go`

### Golden 4 Signals

Metrics follow Google's Golden 4 Signals pattern: Latency, Traffic, Errors, Saturation.

**HTTP & Job Metrics:**

The `path` label is the mux route the request matched (`/v1/jobs/{jobId}`, `/v1/deployments/{id}/traffic`, …), so cardinality is bounded by the route table. Note: the jobs placeholder is whatever the route declares — it was briefly `{id}` while paths were normalized by hand, so dashboards filtering on that value need updating.

| Signal | Metrics |
|--------|---------|
| Latency | `http_request_duration_seconds`, `job_duration_seconds` |
| Traffic | `http_requests_total`, `jobs_total` |
| Errors | `http_errors_total`, `job_duration_seconds_count{success="false"}` |
| Saturation | `jobs_active` |

Job completions and job errors are both read off `job_duration_seconds_count`, which carries `image` and `success` — there is no separate `job_errors_total` to fall out of step with it.

**Dispatcher (Callback) Metrics:**

| Signal | Metrics |
|--------|---------|
| Latency | `dispatcher_duration_seconds` |
| Traffic | `dispatcher_delivered_total` |
| Errors | `dispatcher_failed_total`, `dispatcher_dropped_total` |
| Saturation | `dispatcher_queue_size`, `dispatcher_requeued_total` |

**Kubernetes Backend Metrics:**

Only populated when `ORCHESTRATOR_BACKEND=kubernetes`; for the Docker backend these stay at zero.

| Signal | Metrics |
|--------|---------|
| Leadership | `orchestrator_leader{identity}` gauge, `orchestrator_leader_transitions_total{identity}` counter |
| Cache | `orchestrator_status_cache_hits_total`, `orchestrator_status_cache_misses_total` |
| Saturation | `orchestrator_trackers` (in-flight per-job lifecycle watchers on the leader) |
| K8s API | `k8s_api_request_duration_seconds{verb,resource}`, `k8s_api_errors_total{verb,resource,status}` |

The K8s API metrics cover every call the orchestrator makes to the apiserver — `Run`/`Stop`/`Status`/`List` and the informer's list+watch — instrumented via a `rest.Config.Wrap` transport.

### Saturation metrics are asynchronous gauges

`jobs_active`, `orchestrator_trackers` and `dispatcher_queue_size` are OTel
*observable* gauges: nothing increments them, and a callback reads the live
value (non-terminal jobs, tracker map size, queue length) when Prometheus
scrapes. This is deliberate. Tallying them with an `UpDownCounter` requires one
process to see both the `+1` and the `-1`, and this service never does — a
restart zeroes the counter while the jobs it counted keep running and later
report their exits, and a K8s leadership handover moves the `-1` to a replica
that never did the `+1`. Both leak negative permanently. Read the state instead;
it cannot drift. Register new saturation metrics with
`Metrics.ObserveInt64`, and reach for `UpDownCounter` only when the increment
and decrement are provably in the same function (e.g. a `defer`).

See: `internal/observability/metrics.go`, `internal/job/kubernetes/transport.go`

**Deployments Metrics:**

Both the deployments service and the standalone activator (K8s) expose the same registry; each records the slice it owns.

| Signal | Metrics |
|--------|---------|
| Latency | `deployment_rollout_duration_seconds` (revision minted → traffic cut), `activator_hold_duration_seconds{component,outcome=served\|timeout}` (the client-visible cold-start cost) |
| Traffic | `deployments_applied_total{created}`, `deployment_rollout_cuts_total`, `activator_raises_total{component}`, `activator_async_total{component,result=delivered\|failed}`, `autoscaler_scale_events_total{direction=up\|down}` |
| Errors | `autoscaler_scrape_errors_total` (failed concurrency scrapes while replicas are serving) |
| Saturation | `deployments_active`, `activator_queued{component}` (requests held for capacity), `autoscaler_desired_replicas{deployment}` |

Rollout metrics come from the leader's reconciler; leadership and K8s API metrics apply to the deployments service and activator exactly as to jobs. The broker behind these series runs in two components, and the `component` label says which — `deployments-activator` or `sandbox-proxy`, matching their `app.kubernetes.io/component` labels, so a PromQL series and a pod selector read the same. A sandbox hold never reads as a deployment cold start, and `activator_raises_total` is only ever the activator's: the sandbox proxy has nothing to raise.

<a id="pools"></a>
**Pool Metrics:**

Warm pools serve two consumers — deployment-pool activations and [sandboxes](sandboxes.md) — and both are claim-and-late-bind, so they share one set of series. The `kind` label (`pool` | `sandbox`) says which, and pool ids may repeat across the two config lists without colliding.

| Signal | Metrics |
|--------|---------|
| Latency | `pool_activation_duration_seconds{kind,pool,success}` (claim through serving) |
| Traffic | `pool_activations_total{kind,pool}`, `pool_burst_total{kind,pool,policy=reject\|cold}` |
| Errors | `pool_poisoned_total{kind,pool}` (failed artifact materialization), `pool_claim_conflicts_total{kind,pool}` (lost claim races — healthy at low rates, a hot pool at high ones) |
| Saturation | `pool_activations_active{kind,pool}`, `pool_warm{kind,pool}`, `pool_claimed{kind,pool}` |

Warm/claimed capacity gauges are recorded by the leader's control loop each tick. `pool_warm` dropping to zero while `pool_burst_total{policy="reject"}` climbs is the signal to grow `size`.

### Dispatcher Statistics

The event dispatcher tracks delivery statistics:

| Metric | Description |
|--------|-------------|
| Queue depth | Current number of pending events |
| Queued | Total events added to queue |
| Delivered | Successful deliveries |
| Failed | Failed after all retries |
| Dropped | Dropped due to full buffer |
| Circuit open | Skipped due to open circuit breaker |
| Retries | Total retry attempts |
| Breakers total | Number of circuit breakers |
| Breakers open | Currently open circuit breakers |

See: `internal/dispatcher/dispatcher.go` -> `Stats`

### Metric Labels

Metrics use consistent labels for filtering and aggregation:

| Label | Values | Used By |
|-------|--------|---------|
| `method` | GET, POST, DELETE | HTTP metrics |
| `path` | /v1/jobs/{jobId}, /readyz, other | HTTP metrics |
| `status` | 2xx, 4xx, 5xx | HTTP metrics |
| `image` | alpine:latest, etc. | Job metrics |
| `success` | true, false | Job duration |

### Route Patterns for Cardinality Control

The `path` label is the route pattern the request matched, not the URL it asked for: `/v1/jobs/abc123` -> `/v1/jobs/{jobId}`. Requests that match no route (404) or the wrong method (405) are labelled `other`.

**Why?** Each unique URL would otherwise create a new time series — one per job ID, plus one per path probed by internet background scanners (`/.env`, `/wp-includes/…`). This would exhaust memory in Prometheus.

See: `internal/api/middleware.go` -> `routePattern()`

### Health Check Semantics

- **Liveness** (`/livez`): Always healthy if process is running. Failure = deadlock, trigger restart.
- **Readiness** (`/readyz`): Checks the configured backend (Docker daemon or K8s API server). Failure = remove from load balancer, don't restart.

**Why separate?** A service can be alive but not ready (e.g., Docker daemon unreachable, K8s API server unresponsive). Restarting won't fix external dependencies.

See: `internal/health/checker.go`

## Prometheus Queries

```promql
# HTTP error rate
sum(rate(http_errors_total[5m])) / sum(rate(http_requests_total[5m]))

# HTTP P99 latency
histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))

# Request rate by endpoint
sum(rate(http_requests_total[5m])) by (method, path)

# Job success rate (numerator and denominator from the same exit path;
# ratioing exits against jobs_total creates would skew on every restart)
sum(rate(job_duration_seconds_count{success="true"}[5m])) / sum(rate(job_duration_seconds_count[5m]))

# Active jobs (sum across replicas: on K8s only the leader holds trackers)
sum(jobs_active)

# Job duration P95
histogram_quantile(0.95, sum(rate(job_duration_seconds_bucket[5m])) by (le))
```

## Grafana Dashboard

A pre-configured dashboard is included at `grafana/dashboards/orchestrator.json`. It's automatically provisioned when running `docker compose up`.

**Panels:**

| Row | Panels |
|-----|--------|
| Overview | Active Jobs, Request Rate, Error Rate, Job Success Rate |
| HTTP Metrics | P95 Latency, Request Rate by Endpoint, Latency Percentiles, Errors by Status |
| Job Metrics | Active Jobs by Image, Job Throughput, Job Duration Percentiles |
| Dispatcher Metrics | Callback Throughput, Callback Latency, Queue Size |

## Alerting Recommendations

| Alert | Condition | Severity |
|-------|-----------|----------|
| High error rate | >5% 5xx for 5m | Critical |
| High latency | P99 >1s for 5m | Warning |
| Job backlog | >100 active for 10m | Warning |
| Service down | `up == 0` for 1m | Critical |
| Dispatcher buffer full | dropped events >0 for 1m | Warning |
| Circuit breakers open | >0 open breakers for 5m | Warning |
