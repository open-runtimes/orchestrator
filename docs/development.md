# Development Guide

## Prerequisites

- Go 1.22+
- Docker

All other tools (Task, ko, golangci-lint, air) are managed as Go dependencies.

## Quick Start

```bash
alias task="go run github.com/go-task/task/v3/cmd/task@latest"
task dev    # Start with hot reload
task test   # Run tests
task lint   # Run linter
task        # Show all available tasks
```

See `Taskfile.yml` for all available tasks.

## Project Layout

```
cmd/              # Entry points
  jobs-service/   # Main API server
  job-sidecar/    # Sidecar for I/O handling
internal/         # Private packages
  api/            # HTTP handlers, middleware, routing
  config/         # Environment-based configuration helpers
  dispatcher/     # Async event dispatch with retry and circuit breaker
  orchestrator/   # Orchestrator implementations (docker/)
  health/         # Liveness/readiness checks
  job/            # Orchestrator interface, validation, types, event builders
  observability/  # Prometheus metrics
  sidecar/        # Input download, output upload handlers
pkg/              # Public packages
  circuitbreaker/ # Per-host circuit breaker implementation
  cloudevent/     # CloudEvents 1.0 types and HTTP sender
e2e/              # End-to-end tests
```

## Configuration

Configuration is loaded from environment variables. Each package manages its own config:

| Package | Env Prefix | Description |
|---------|------------|-------------|
| `config` | `PORT`, `METRICS_PORT`, etc. | Service-level settings |
| `dispatcher` | `DISPATCHER_*` | Event dispatch buffer, workers, retry |
| `docker` | `JOB_RETENTION`, `CALLBACK_PROXY_URL`, etc. | Job retention, callback routing |
| `sidecar` | `JOB_ID`, `CALLBACK_*`, etc. | Sidecar runtime settings |

### Dispatcher Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `DISPATCHER_BUFFER_SIZE` | 10000 | Pending events buffer size |
| `DISPATCHER_WORKERS` | 10 | Concurrent delivery goroutines |
| `DISPATCHER_HTTP_TIMEOUT` | 10s | Per-request timeout |
| `DISPATCHER_MAX_RETRIES` | 3 | Max retry attempts |
| `DISPATCHER_INITIAL_BACKOFF` | 100ms | Initial retry backoff |
| `DISPATCHER_MAX_BACKOFF` | 5s | Max retry backoff |
| `DISPATCHER_BREAKER_THRESHOLD` | 5 | Failures before circuit opens |
| `DISPATCHER_BREAKER_COOLDOWN` | 30s | Time before half-open state |
| `DISPATCHER_MAX_REQUEUES` | 10 | Max requeues when circuit open |

### Callback Proxy Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CALLBACK_PROXY_URL` | `http://host.docker.internal:8080` | Internal URL for routing sidecar callbacks through orchestrator |

Sidecar callbacks are proxied through the orchestrator's dispatcher by default. This enables circuit breaker and retry logic for all callbacks. Set to empty string to disable (sidecar sends directly to callback server).

## Testing

| Command | Scope | Docker Required |
|---------|-------|-----------------|
| `task test` | Unit tests | No |
| `task test-integration` | Docker adapter | Yes |
| `task test-e2e` | Full HTTP API | Yes |

Design decision: Unit tests mock the interfaces and have wide coverage. Integration tests use real Docker. E2E tests run the full system, and focus on happy paths to remain fast.

Key test files:
- `internal/orchestrator/docker/state_test.go` - State repository with concurrency tests
- `internal/dispatcher/memory_test.go` - Dispatcher with retry/circuit breaker tests
- `internal/sidecar/*_test.go` - Input/output handlers with retry scenarios
- `internal/job/service_test.go` - Request validation edge cases
- `pkg/circuitbreaker/*_test.go` - Circuit breaker state transitions

## Observability

Start the monitoring stack:

```bash
docker compose up -d
task dev  # Start service with hot reload
```

- **Grafana**: http://localhost:3000 (no login required)
- **Prometheus**: http://localhost:9091
- **Metrics endpoint**: http://localhost:9090/metrics

The Grafana dashboard shows HTTP request rates, latencies, job throughput, and active jobs.

## Debugging

```bash
# List managed containers
docker ps --filter "label=managed-by=jobs-service"

# Job container logs
docker logs job-<id>-worker
docker logs job-<id>-sidecar
```

All containers are labeled with `job.id`, `job.type`, and `managed-by` for easy filtering.
