# Development Guide

## Prerequisites

- Go 1.25+
- Docker (for the Docker backend, integration tests, and ko image builds)
- Optional, for the K8s dev loop: [kind](https://kind.sigs.k8s.io/), [tilt](https://tilt.dev/)

`task` is bootstrapped via `go run`. Other tools (`ko`, `golangci-lint`, `helm`) are installed into `./bin/` by `task tools` — an idempotent wrapper around `hack/install-tools.sh`.

## Quick Start

```bash
alias task="go run github.com/go-task/task/v3/cmd/task@latest"
task tools    # Install pinned ko, golangci-lint, helm into ./bin/
task dev      # Start with hot reload (Docker backend)
task test     # Run tests
task lint     # Run linter
task          # Show all available tasks
```

For the K8s dev loop see the [K8s development](#kubernetes-development) section.

See `Taskfile.yml` for the full list.

## Project Layout

```
cmd/              # Entry points
  jobs-service/   # Main API server
  job-sidecar/    # Sidecar for I/O handling (supports -mode={combined|pre|post})
internal/         # Private packages
  api/            # HTTP handlers, middleware, routing
  config/         # Environment-based configuration helpers
  dispatcher/     # Async event dispatch with retry and circuit breaker
  orchestrator/   # Orchestrator implementations
    docker/       # Docker backend
    kubernetes/   # Kubernetes backend (batch/v1.Job + native sidecar)
  health/         # Liveness/readiness checks
  job/            # Orchestrator interface, validation, types, event builders
  observability/  # Prometheus metrics
  sidecar/        # Input download, output upload handlers
  circuitbreaker/ # Per-host circuit breaker implementation
  cloudevent/     # CloudEvents 1.0 types and HTTP sender
  lifecycle/      # Run-to-completion state machine (signals, FSM, in-memory store)
  workload/       # The workload-sidecar contract: ports, env, claim payloads
  proxy/          # The workload sidecar itself
charts/           # Helm chart (charts/orchestrator/)
hack/             # Dev-only assets: install-tools.sh, kind-config.yaml, dev-values.yaml
Tiltfile         # Live-reload dev loop against the kind cluster
e2e/              # End-to-end tests
```

## Configuration

Configuration is loaded from environment variables. Each package manages its own config:

| Package | Env Prefix | Description |
|---------|------------|-------------|
| `config` | `PORT`, `METRICS_PORT`, `ORCHESTRATOR_BACKEND`, etc. | Service-level settings; backend selection |
| `dispatcher` | `DISPATCHER_*` | Event dispatch buffer, workers, retry |
| `orchestrator/docker` | `JOB_RETENTION`, `MAINTENANCE_INTERVAL`, `ARTIFACT_ENDPOINT`, `EXTRA_HOSTS` | Docker backend config |
| `orchestrator/kubernetes` | `KUBE_NAMESPACE`, `KUBE_JOB_SERVICE_ACCOUNT`, `KUBE_IMAGE_PULL_SECRETS`, `KUBE_TERMINATION_GRACE_SECONDS`, `KUBECONFIG`, plus shared `JOB_RETENTION`/`MAINTENANCE_INTERVAL`/`ARTIFACT_ENDPOINT` | Kubernetes backend config |
| `sidecar` | `JOB_ID`, `CALLBACK_*`, etc. | Sidecar runtime settings |

`ORCHESTRATOR_BACKEND` selects the backend (`docker` default, or `kubernetes`).

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
| `task test` | Unit tests (incl. K8s backend with fake clientset) | No |
| `task test-integration` | Docker adapter against real daemon | Yes |
| `task test-e2e` | Full HTTP API via Docker backend | Yes |

Design decision: Unit tests mock the interfaces and have wide coverage. Integration tests use real Docker; the K8s backend has unit tests via `k8s.io/client-go/kubernetes/fake` but no real-cluster integration tests yet. E2E tests run the full system over the Docker backend and focus on happy paths to remain fast.

Key test files:
- `internal/orchestrator/docker/docker_integration_test.go` - Docker adapter against real daemon
- `internal/orchestrator/docker/{mapper,watcher}_test.go` - Docker mapping and event watcher
- `internal/orchestrator/kubernetes/{mapper,kubernetes}_test.go` - K8s mapping + fake-clientset coverage
- `internal/dispatcher/memory_test.go` - Dispatcher with retry/circuit breaker tests
- `internal/sidecar/*_test.go` - Input/output handlers with retry scenarios
- `internal/job/service_test.go` - Request validation edge cases
- `internal/circuitbreaker/*_test.go` - Circuit breaker state transitions

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

## Kubernetes development

The `Tiltfile` drives a live-reload loop against a local [kind](https://kind.sigs.k8s.io/) cluster called `orchestrator-dev`. Tilt watches Go sources, rebuilds via `ko`, and redeploys the Helm chart automatically.

```bash
task tools       # install pinned ko, golangci-lint, helm into ./bin/
task kind:up     # create the kind cluster
task dev:k8s     # tilt up
```

Port-forwards are set in the Tiltfile: API on `localhost:8080`, metrics on `localhost:9090`.

To render or lint the chart without a cluster:

```bash
task helm:lint
task helm:template
```

## Debugging

Docker backend:

```bash
# List managed containers
docker ps --filter "label=managed-by=jobs-service"

# Job container logs
docker logs job-<id>-worker
docker logs job-<id>-sidecar
```

All containers are labeled with `job.id`, `job.type`, and `managed-by` for easy filtering.

Kubernetes backend (against the kind dev cluster):

```bash
task k8s:jobs          # list managed Jobs + Pods
task k8s:logs          # tail jobs-service logs

# Worker or sidecar logs for a specific job
kubectl --context kind-orchestrator-dev -n orchestrator \
  logs job/job-<id> -c worker        # or -c artifact-pre / artifact-post
```

All Jobs and Pods carry `managed-by=jobs-service` and `job.id=<id>` labels.
