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
- `cmd/deployments-service` — serving plane (deployments + pools): API + in-process activator data plane
- `cmd/sandboxes-service` — sandbox control plane: /v1/sandbox API; on Docker also the in-process data plane (token-routed proxy on its own data port)
- `cmd/workload-sidecar` — reverse proxy in front of every serving workload: deployment replicas, pool activations, and sandboxes (readiness, drain, per-request timeout, request counting, and the claim endpoint)
- `cmd/deployments-activator` — K8s buffering data plane for deployments: holds cold/async traffic (gateway routes here with X-Revision) and raises cold revisions
- `cmd/sandbox-proxy` — K8s data plane for sandboxes: one wildcard route, resolved by the capability token in the Host. Its own component, not an activator mode — always on the path, pods-read-only, nothing to raise
- `cmd/pool-shim` — warm-pod entrypoint: blocks on a FIFO, execs the activation payload as PID 1
- `internal/` — everything. A service exposes binaries, not packages, so nothing here is importable from outside the module (there is no `pkg/`: it is not a Go standard, and `internal/` is the compiler-enforced one). Every domain package parents its own adapters: `internal/job/{docker,kubernetes}`, `internal/deployment/{docker,kubernetes}`, `internal/pool/kubernetes`, `internal/sandbox/{docker,kubernetes}` — the directory says which domain a backend implements.
  - Domain + API types (each with its backends beneath): job, deployment, pool, sandbox; plus artifact, volume, lifecycle (run-to-completion FSM)
  - Machinery: api, server, warm (warm-pool engine: pods, claim, replenish, GC — shared by pools and sandboxes), claim (the claim protocol), workload (the workload-sidecar contract), proxy (that sidecar), sidecar (artifact runner), activator, autoscaler, dispatcher, kube, config, observability, apperrors
  - Utilities with no dependency on any of the above: backoff, circuitbreaker, cloudevent, emitter
- `charts/orchestrator/` — Helm chart
- `hack/` — dev-only assets (kind config, dev values, install-tools.sh)
- `Tiltfile` — live-reload dev loop against kind

## Key concepts

- Jobs run in Docker containers or Kubernetes `batch/v1.Job` resources; artifacts are ordered by `depends` field
- Sandboxes are live workspaces claimed from warm pools or created cold by naming an image (pools are optional warm capacity); exec and files are an HTTP contract served inside the sandbox by the open-runtimes/sandbox agent, which an init container copies into the workspace — so any runtime image works and a pool needs no `command`. A sandbox's hostname carries an unguessable capability token
- K8s backend uses a native sidecar (K8s 1.29+) — kubelet sends SIGTERM to the sidecar when the worker exits
- Callbacks use CloudEvents 1.0 with optional HMAC-SHA256 signing
- Service survives restarts — in-flight jobs are resumed
