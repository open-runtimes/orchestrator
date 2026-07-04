# Implementation Plan

Maps the [delivery phases](deployments-service.md#delivery-phases) onto the existing codebase. Each
phase ships independently (green CI, demoable), and the reuse claims below reference the real
packages, not intentions. Total existing source is ~7.5k LoC; the discipline is that every phase
starts by asking what it can import or extract before what it must write.

## What we reuse, verbatim or extracted

| Need | Existing code | How |
|------|---------------|-----|
| HTTP server wiring, graceful shutdown | `pkg/server/run.go` | generalize `Run()` to take a router, not job handlers |
| Middleware chain (recovery→logging→metrics→CORS→content-type→auth) | `internal/api/middleware.go` | import as-is; only routes are new |
| Generic state store + FSM | `pkg/job/controller.go:48` (`MemoryStore[T]`), `pkg/job/lifecycle.go:11` (`Signal`) | **Phase 0 extraction** → `pkg/lifecycle`, `pkg/job` re-exports |
| CloudEvents delivery (HMAC, retry, circuit-break) | `internal/dispatcher` (`HTTPSender`→`WithRetry`→`WithCircuitBreaker`) | import as-is; new event types only |
| Callback fan-out | `pkg/emitter` (`Emitter[T]`) | import as-is |
| Artifact pipeline | `internal/artifact` (`Registry`, 8 types incl. squashfs mount) | import as-is in sidecar modes |
| Artifact materialization in workload pods | `cmd/job-sidecar` (`-mode=pre`) | reused as the `artifact-pre` container; the deployments proxy is its own binary |
| K8s client, informers, leader election | `internal/orchestrator/kubernetes` (lease-based, `LeaderElectionConfig`) | extract client/lease setup into a shared sub-package |
| Backend selection + config | `internal/config`, `ORCHESTRATOR_BACKEND` factory pattern in `cmd/jobs-service/main.go` | same pattern, new factory |
| Helm chart, kind, Tilt, CI k8s-integration job | `charts/orchestrator`, `hack/`, `Tiltfile`, `.github/workflows/ci.yml` | extend, don't fork |

## Binary & process model

**One dedicated binary per component — no mode/role flags.** `cmd/deployments-service` (HTTP API,
reconcilers, the leader-elected autoscaler goroutine, and — until Phase 3 — the in-process activator
data plane) and `cmd/deployments-sidecar` (the per-replica reverse proxy, a thin main over
`internal/proxy`). Phase 3 adds `cmd/deployments-activator` as its own binary when the buffering edge
becomes a separate gateway-targeted Deployment, and Phase 5 adds a dedicated pool-shim binary.
`job-sidecar` keeps artifact materialization only (its pre/post/combined modes are phases of one job
workflow, not different programs) and remains the `artifact-pre` image for deployments.

## Phase 0 — extraction & scaffolding (net-negative target)

Refactors that keep the jobs service green while making the serving plane importable, plus the empty
shell. No new behavior.

- Extract `MemoryStore[T]`, `Entry`, `Signal` from `pkg/job` → `pkg/lifecycle`; jobs keeps thin
  aliases. Extract K8s client/lease/informer setup from `internal/orchestrator/kubernetes` into a
  shared sub-package both backends use.
- Generalize `pkg/server.Run()` to accept a router (jobs passes its current one).
- `cmd/deployments-service` skeleton: config load, backend factory stub, middleware chain, `/livez` /
  `/readyz`, metrics port. Helm: a disabled-by-default `deployments` component (Deployment, Service,
  SA, RBAC); `task dev` / Tilt wire it behind a flag.
- **Exit:** existing tests pass untouched (only import paths change); new binary builds, deploys to
  kind, answers probes. This phase is where the deletion opportunities live — take them.

## Phase 1 — deployments MVP (Docker + K8s)

A container spec becomes an HTTP-addressable workload at a stable URL, sync + async, fixed replicas,
single revision. All traffic through the in-process activator (no gateway yet).

- `pkg/deployment`: `Request`/`StatusResponse`/`Probes` types, the `Orchestrator` interface
  ([orchestrator.md](orchestrator.md)), `Service` mirroring `pkg/job.Service` on
  `pkg/lifecycle.MemoryStore`.
- `cmd/deployments-sidecar` (over `internal/proxy`): reverse proxy, sub-second readiness probe +
  health endpoint, `preStop` drain (grace = `min(timeoutSeconds, maxDrainSeconds)`), in-flight counter
  (exposed now, scraped in Phase 4), `concurrency` cap + bounded queue → `503`.
- Backends: **Docker** — sidecar container fronting the user container on `DOCKER_NETWORK`,
  `artifact-pre` step, `Apply` recreates on spec change. **K8s** — one `apps/v1.Deployment` + one
  (selector-ful, for now) Service; pod template = `artifact-pre` init + native `proxy` sidecar + user
  container, under the full hardened SecurityContext floor from [security.md](security.md) (cheap
  now, painful to retrofit).
- In-process activator: virtual-host routing by `host`, forwards to sidecar; `Prefer: respond-async`
  → `202` + `X-Invocation-Id` + `orchestrator.deployment.response` via the dispatcher (1 MiB cap).
- API: `POST/GET/DELETE /v1/deployments`, `GET /{id}`; `Reconcile()` on start from labels/objects.
- **Exit:** e2e (docker tag): deploy → curl URL → async → callback received → delete. k8s_integration:
  same against kind. Failure cases: never-ready (`progressDeadlineSeconds` → `failed`), drain
  under load, `503` load-shed.

## Phase 2 — scale-to-zero (0↔1, both backends)

- Activator: buffer on cold hit, raise `0→1` itself (K8s: `scale` subresource patch — its only write
  RBAC; Docker: `Scale(_,1)`), direct-probe the sidecar health endpoint, release to pod IP on first
  success; `responseStartTimeoutSeconds` → `503`; bounded buffer.
- Idle-to-zero: a minimal loop (the Phase 4 autoscaler's seed) watches sidecar in-flight counters;
  zero for the stable window → `Scale(_,0)`.
- **Exit:** e2e: idle → observe zero pods/containers → request → response (one cold start, no error)
  → warm again. Race tests: concurrent cold hits, crash-during-cold-start.

## Phase 3 — Gateway API: revisions, traffic, off-path (K8s)

The largest phase; the serving plane becomes real.

- Revisions: immutable snapshots (`{id}-0000N`), spec-hash on `POST`, per-revision Deployment +
  **selectorless** Service; marker ConfigMap (scaling bounds, shard assignment, last-ready);
  `Retire()` GC with `revisionHistoryLimit: 3`; auto-cut-on-ready + rollout protection
  ([failure-semantics](failure-semantics.md)).
- `HTTPRoute` adapter: weighted `backendRefs` with per-backendRef `X-Revision` (`set`) filters +
  the exact-literal `Prefer: respond-async` rule; `host` ownership (`409` on collision).
- **EndpointSlice reconciler (cold flip)** — ready pods ↔ activator, correct target ports; driven by
  ready-count; leader-elected. The subtlest code in the plan: property-test the reconciler in
  isolation (fake informers) before wiring it.
- Activator becomes `ROLE=activator` Deployment behind the gateway (gateway-only ingress netpol),
  trusting `X-Revision`; `POST /traffic` API.
- Dev/CI: Gateway API CRDs + **Traefik** in `hack/kind` + Tilt + the CI k8s-integration job.
- **Exit:** k8s_integration: canary 90/10 with the 10% revision cold (buffered via activator, split
  preserved); rollback via `/traffic`; kill last pod → `502`-free recovery through the flip; upgrade
  with in-flight requests (drain). Conformance smoke against Envoy Gateway to keep the portability
  claim honest.

## Phase 4 — concurrency autoscaler (1↔N, K8s)

- Leader-elected goroutine in the `api` role (reuses the job lease pattern): 2s tick, 60s sliding
  window, `desired = clamp(ceil(avg/target))` → `scale` patch. Scrapes sidecar in-flight counters
  (endpoint exists since Phase 1) + activator queue depth to hold-up/scale past 1.
- Replica-aware PDBs + `topologySpreadConstraints` from [resource-model](resource-model.md);
  requests-from-limits derivation (overcommit ratios, `LimitRange` per namespace).
- **Exit:** load test in kind: step load → replicas track concurrency; drop to zero → scale-to-zero;
  burst → shed bounded by tick not window. Balloon pods / descheduler / surge controller stay
  **out** — operational add-ons, not service code.

## Phase 5 — pools (Docker + K8s)

Depends only on Phase 1 + the shim; can start in parallel once Phase 3 is underway if product
pressure demands — the claim protocol doesn't touch the gateway until HTTP-mode activations.

- `pkg/pool`: config schema (Helm `pools:` list), `Activation` types.
- A dedicated pool-shim binary: block on FIFO, `exec` payload as PID 1. The deployments-sidecar grows the
  activation surface: claim POST (per-pod token → `401`; first-wins → `409`), artifact
  materialization via `internal/artifact`, readiness gate.
- Replenishment controller (leader-elected): `warm_count → size`, off-path; claimed-but-unlabeled
  TTL GC; burst fallback (cold-create or `429`).
- API: read + activate; exec mode inline results (no Service/route); HTTP mode per-activation
  Service + `HTTPRoute`, idle teardown.
- **Exit:** e2e: activate (exec) → exit code/output inline; async → `.result` callback; HTTP mode →
  serve at URL → idle teardown → slot replenished. Race: N concurrent activates over M<N warm pods
  → exactly M win, rest overflow per config. Chaos: kill service mid-claim → orphan GC'd, never
  resold. Measure claim→ready and route-propagation latency (the numbers [pools.md](pools.md)
  promises).

## Phase 6 — production hardening (K8s, cross-cutting)

Everything [security](security.md)/[resource-model](resource-model.md) defer beyond the pod floor:
shard namespaces + least-loaded assignment, `ClusterwideNetworkPolicy` (incl. metadata-endpoint
block), PSA `restricted` labels, `gvisor`/`kata` RuntimeClass validation + per-runtime pools,
`ResourceQuota`, revision-GC pressure testing toward the scale table. Gated on real deployment
targets; not a blocker for 1–5.

## Risks & checkpoints

- **Traefik per-backendRef filters** — verified in docs (Gateway API v1.5.1); **prove in kind during
  Phase 3 week 1**, before the adapter is built on it. Fallback: Envoy Gateway.
- **EndpointSlice reconciler correctness** — the one novel-ish component (everything else has a
  Knative or jobs-service precedent). Isolate + property-test first.
- **Sidecar image growth** — `proxy`/`shim` modes must not bloat the init-container path jobs use;
  watch image size in CI.
- **`pkg/server`/`pkg/job` extraction churn** — Phase 0 touches the jobs service; keep it
  mechanical, land it as its own PR, no behavior change.
