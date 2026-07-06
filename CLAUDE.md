# Orchestrator

Job orchestration service for running containerized workloads with async callbacks. Supports Docker and Kubernetes backends (selected via `ORCHESTRATOR_BACKEND`).

## Commands

- `task tools` — install pinned ko, golangci-lint, helm into `./bin/`
- `task dev` — run locally with hot reload (Docker backend)
- `task test` — unit tests
- `task test-integration` — integration tests
- `task test-e2e` — end-to-end tests
- `task lint` — lint
- `task fmt` — format
- `task build` — build OCI images (jobs-service + job-sidecar) via ko
- `task helm:lint` / `task helm:template` — validate the Helm chart
- `task kind:up` / `task kind:down` — manage the kind dev cluster
- `task dev:k8s` — live-reload K8s dev loop (`tilt up`)

## Structure

- `cmd/jobs-service` — main orchestration service (HTTP API on :8080, metrics on :9090)
- `cmd/job-sidecar` — sidecar for artifact processing and job lifecycle
- `cmd/deployments-service` — serving plane (deployments + pools): API + in-process activator data plane, see `docs/design/`
- `cmd/deployments-sidecar` — reverse proxy in every deployment replica (readiness, drain, concurrency cap)
- `cmd/deployments-activator` — K8s buffering edge for cold/async traffic (gateway routes here with X-Revision)
- `cmd/pool-shim` — warm-pod entrypoint: blocks on a FIFO, execs the activation payload as PID 1
- `internal/` — core packages: api, job, artifact, dispatcher, kube (shared K8s client/leader election), orchestrator/{docker,kubernetes}, sidecar, config
- `pkg/` — reusable utilities: backoff, circuitbreaker, cloudevent, lifecycle (shared workload FSM/store), server
- `charts/orchestrator/` — Helm chart
- `hack/` — dev-only assets (kind config, dev values, install-tools.sh)
- `Tiltfile` — live-reload dev loop against kind

## Key concepts

- Jobs run in Docker containers or Kubernetes `batch/v1.Job` resources; artifacts are ordered by `depends` field
- K8s backend uses a native sidecar (K8s 1.29+) — kubelet sends SIGTERM to the sidecar when the worker exits
- Callbacks use CloudEvents 1.0 with optional HMAC-SHA256 signing
- Service survives restarts — in-flight jobs are resumed
