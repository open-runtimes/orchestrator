# Orchestrator

A service for running containerized workloads on Docker or Kubernetes, in two shapes:

- **Jobs** — run a container to completion, with file artifacts in and out, and CloudEvents callbacks reporting progress and results.
- **Deployments** — run a container as a long-lived HTTP service behind a gateway, with immutable revisions, traffic splitting, concurrency-based autoscaling, and scale-to-zero. **Pools** keep pre-warmed capacity for near-instant activations.

The same API works against both backends (`ORCHESTRATOR_BACKEND=docker|kubernetes`): Docker for development, Kubernetes for production. The backend is the source of truth — the services are stateless, survive restarts, and any replica can serve any request.

## Quick start

The [compose file](docker-compose.yaml) runs both services against your local Docker daemon:

```bash
docker compose up -d

# Run a job to completion
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"id": "hello", "image": "alpine:latest", "command": "echo hello world"}'
curl http://localhost:8080/v1/jobs/hello
# {"id":"hello","status":"completed","exitCode":0}

# Deploy an HTTP service
curl -X POST http://localhost:8082/v1/deployments \
  -H "Content-Type: application/json" \
  -d '{"id": "web", "image": "traefik/whoami", "port": 80}'
# 201 {"id":"web","status":"pending","url":"http://web.localhost", ...}

# Once ready, it serves on its host via the data port:
curl -H "Host: web.localhost" http://localhost:8081/
```

(Contributors can also run the jobs service from source with hot reload: `task dev`.)

## Documentation

| Guide | What it covers |
| --- | --- |
| [Jobs](docs/jobs.md) | Run-to-completion workloads: the jobs API, artifacts (download, write, archive, mount, …), dependency ordering |
| [Deployments](docs/deployments.md) | Long-lived HTTP services: revisions, canary traffic, autoscaling, scale-to-zero, async requests |
| [Pools](docs/pools.md) | Pre-warmed capacity: configuring pools, activations, burst policy |
| [Sandboxes](docs/sandboxes.md) | Live workspaces: the sandbox API, the in-sandbox exec/files contract, extra ports, sandbox pools, isolation tiers |
| [Callbacks](docs/callbacks.md) | CloudEvents delivery: every event type, payload schemas, HMAC signature verification |
| [Operations](docs/operations.md) | Deploying the orchestrator: Helm install, prerequisites, configuration reference, hardening |
| [Observability](docs/observability.md) | Metrics, logging, and tracing |
| [Development](docs/development.md) | Building, testing, and the local dev loop |

Proposals for work not yet built live in [`docs/design/`](docs/design/).

## API at a glance

All request and response bodies are JSON; every error is `{"error": "..."}` with a meaningful status code. Requests with unknown fields are rejected with `400` naming the field — a typo never silently deploys defaults. When an API key is configured, send `Authorization: Bearer <key>`.

```
POST   /v1/jobs                                    # 202 — run a container to completion
GET    /v1/jobs/{id}                               # status + exit code
DELETE /v1/jobs/{id}                               # cancel

POST   /v1/deployments                             # 201 created / 200 updated (declarative apply)
GET    /v1/deployments/{id}                        # status, revisions, traffic, mode
POST   /v1/deployments/{id}/traffic                # canary / rollback; empty targets = back to auto
DELETE /v1/deployments/{id}                        # tear down

GET    /v1/deployment-pools                        # configured pools + warm counts
POST   /v1/deployment-pools/{id}/activations       # 201 — claim a warm pod, serve HTTP
DELETE /v1/deployment-pools/{id}/activations/{aid} # deactivate
```

## Deploy to Kubernetes

A Helm chart lives at `charts/orchestrator/` — see the [operations guide](docs/operations.md) for prerequisites (K8s 1.29+; Gateway API for deployments) and the full configuration reference.

```bash
helm install orchestrator oci://ghcr.io/open-runtimes/charts/orchestrator \
  --version <X.Y.Z> \
  --namespace orchestrator --create-namespace \
  --set jobs.enabled=true --set deployments.enabled=true \
  --set deployments.activator.enabled=true
```

Local dev loop (requires kind + tilt):

```bash
task tools       # install pinned ko, golangci-lint, helm into ./bin/
task kind:up     # create the kind-orchestrator-dev cluster
task dev:k8s     # tilt up: live-reload the chart on source change
```

## License

MIT
