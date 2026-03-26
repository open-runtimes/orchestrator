# Orchestrator

Job orchestration service for running containerized workloads with async callbacks.

## Commands

- `task dev` — run locally with hot reload
- `task test` — unit tests
- `task test-integration` — integration tests
- `task test-e2e` — end-to-end tests
- `task lint` — lint
- `task fmt` — format
- `task build` — build Docker images (jobs-service + job-sidecar)

## Structure

- `cmd/jobs-service` — main orchestration service (HTTP API on :8080, metrics on :9090)
- `cmd/job-sidecar` — sidecar for artifact processing and job lifecycle
- `internal/` — core packages: api, job, artifact, dispatcher, orchestrator, sidecar, config
- `pkg/` — reusable utilities: backoff, circuitbreaker, cloudevent

## Key concepts

- Jobs run in Docker containers; artifacts are ordered by `depends` field
- Callbacks use CloudEvents 1.0 with optional HMAC-SHA256 signing
- Service survives restarts — in-flight jobs are resumed
