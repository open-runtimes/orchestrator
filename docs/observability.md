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
- `localhost:8080/readyz` - Readiness (Docker reachable)

## Design Decisions

### Separate Metrics Port

Metrics are served on port 9090, separate from the API on 8080.

**Why?** Allows different access controls. Metrics endpoints can be internal-only while API is exposed. Also prevents metrics scraping from affecting API latency measurements.

See: `cmd/jobs-service/main.go`

### Golden 4 Signals

Metrics follow Google's Golden 4 Signals pattern: Latency, Traffic, Errors, Saturation.

**HTTP & Job Metrics:**

| Signal | Metrics |
|--------|---------|
| Latency | `http_request_duration_seconds`, `job_duration_seconds` |
| Traffic | `http_requests_total`, `jobs_total` |
| Errors | `http_errors_total`, `job_errors_total` |
| Saturation | `jobs_active` |

**Dispatcher (Callback) Metrics:**

| Signal | Metrics |
|--------|---------|
| Latency | `dispatcher_duration_seconds` |
| Traffic | `dispatcher_delivered_total` |
| Errors | `dispatcher_failed_total`, `dispatcher_dropped_total` |
| Saturation | `dispatcher_queue_size`, `dispatcher_requeued_total` |

See: `internal/observability/metrics.go`

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
| `path` | /v1/jobs/{jobId}, /readyz, etc. | HTTP metrics |
| `status` | 2xx, 4xx, 5xx | HTTP metrics |
| `image` | alpine:latest, etc. | Job metrics |
| `success` | true, false | Job duration |

### Path Normalization for Cardinality Control

Request paths are normalized to prevent cardinality explosion: `/v1/jobs/abc123` -> `/v1/jobs/{jobId}`.

**Why?** Without normalization, each unique job ID creates a new time series. This would exhaust memory in Prometheus.

See: `internal/observability/attributes.go` -> `normalizePath()`

### Health Check Semantics

- **Liveness** (`/livez`): Always healthy if process is running. Failure = deadlock, trigger restart.
- **Readiness** (`/readyz`): Checks Docker daemon. Failure = remove from load balancer, don't restart.

**Why separate?** A service can be alive but not ready (e.g., Docker daemon unreachable). Restarting won't fix external dependencies.

See: `internal/health/checker.go`

## Prometheus Queries

```promql
# HTTP error rate
sum(rate(http_errors_total[5m])) / sum(rate(http_requests_total[5m]))

# HTTP P99 latency
histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))

# Request rate by endpoint
sum(rate(http_requests_total[5m])) by (method, path)

# Job success rate
1 - (sum(rate(job_errors_total[5m])) / sum(rate(jobs_total[5m])))

# Active jobs by image
sum(jobs_active) by (image)

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
