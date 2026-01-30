# Orchestrator

A job orchestration service for running containerized workloads with callbacks.

## Features

- Async job execution in Docker containers
- Unified artifacts system (downloads, uploads, file writes, archives)
- Dependency-based artifact ordering (pre-job and post-job)
- CloudEvents 1.0 callbacks with HMAC-SHA256 signing
- Async event dispatch with circuit breaker and retry
- Prometheus metrics (Golden 4 Signals)
- Restart resilience (jobs survive service restarts)

## Quick Start

```bash
# Prerequisites: Go 1.22+, Docker

# Run with hot reload
go run github.com/go-task/task/v3/cmd/task@latest dev

# Create a job
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "id": "hello-world",
    "image": "alpine:latest",
    "command": "echo hello world"
  }'

# Check status
curl http://localhost:8080/v1/jobs/hello-world
```

## API

### Create Job

```
POST /v1/jobs
```

Minimal example:
```json
{
  "id": "my-job",
  "image": "alpine:latest",
  "command": "cat data/input.txt > data/output.txt"
}
```

Full example with artifacts and callbacks:
```json
{
  "id": "my-job",
  "image": "python:3.12-slim",
  "command": "python scripts/process.py",
  "cpu": 2,
  "memory": 512,
  "timeoutSeconds": 300,
  "environment": {
    "LOG_LEVEL": "debug"
  },
  "artifacts": [
    {
      "id": "script",
      "type": "write",
      "in": "print('hello')",
      "out": "scripts/process.py"
    },
    {
      "id": "data",
      "type": "download",
      "in": "https://example.com/input.txt",
      "out": "data/input.txt"
    },
    {
      "id": "result",
      "type": "upload",
      "in": "data/output.txt",
      "out": "https://example.com/upload",
      "depends": "job"
    },
    {
      "id": "metrics",
      "type": "read",
      "in": "data/metrics.json",
      "depends": "job"
    }
  ],
  "callback": {
    "url": "https://example.com/webhook",
    "events": ["orchestrator.job.exit"],
    "key": "hmac-secret"
  }
}
```

### Other Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/jobs` | GET | List all jobs |
| `/v1/jobs/:id` | GET | Get job status |
| `/v1/jobs/:id` | DELETE | Cancel job |
| `/livez` | GET | Liveness probe |
| `/readyz` | GET | Readiness probe |
| `/metrics` | GET | Prometheus metrics (port 9090) |

## Callback Events

CloudEvents 1.0 format, optionally signed with HMAC-SHA256.

| Event | Source | Description |
|-------|--------|-------------|
| `orchestrator.job.start` | service | Job started |
| `orchestrator.job.log` | service | Log output |
| `orchestrator.job.exit` | service | Job exited |
| `orchestrator.job.artifact` | sidecar | Artifact processed |

## Documentation

- [Development Guide](docs/development.md) - Setup, testing, debugging
- [Architecture](docs/architecture.md) - Design decisions
- [Observability](docs/observability.md) - Metrics and health checks

## License

MIT
