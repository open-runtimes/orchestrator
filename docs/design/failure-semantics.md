# Failure Semantics

Cross-cutting; adapted from Knative. The governing rule: **readiness is the source of truth** —
traffic only ever reaches a ready endpoint, and only ever advances to a ready revision.

## Status-code contract

Set by the data plane, never guessed:

| Code | Meaning |
|------|---------|
| `504` | an accepted request to a ready container exceeded `timeoutSeconds` (upstream too slow) |
| `503` | no serving capacity became ready — the activator's `responseStartTimeoutSeconds` wait expired, or `concurrency`/queue full |
| `502` | no reachable endpoint — should not *escape* while the cold endpoint flip is working (the activator stays a member of the revision's endpoint set); a `502` indicates an endpoint-management / propagation lapse |
| app status | application `4xx`/`5xx` pass through unchanged — **never retried** (idempotency) |

The data plane **retries connection-level failures** (dial errors, a pod going away) onto another
endpoint and **buffers cold starts**, but a genuine application `5xx` is returned as-is.

## Modes

- **Revision never becomes ready.** A revision must report ready within `progressDeadlineSeconds`
  (default 600s) — mapped onto the backing Deployment's `spec.progressDeadlineSeconds`, so the signal
  is k8s-native. Before the deadline → `pending` (`degraded` if some but not all replicas ready); past
  it → `failed`, with the container reason in `Error` (`ImagePullBackOff`, `CrashLoopBackOff`,
  `ExitCode137`, …).
- **Rollout protection (no auto-rollback).** The deployment tracks its **last-ready revision**; the
  auto-cut shifts traffic to a new revision *only* once it reports ready, so a failed new revision
  **never receives traffic**. Stuck-rollout signal: `latest-created ≠ last-ready`. Rollback is a
  manual `POST /traffic`; nothing reverts automatically.
- **Cold-start failure.** The [deployments-activator](deployments-activator.md) buffers; no ready
  endpoint within `responseStartTimeoutSeconds` (300s) → `503`. Buffer bounded; overflow → `503`.
- **Overload.** `concurrency` (hard per-pod cap) + a bounded
  [deployments-sidecar](deployments-sidecar.md) queue; both full → `503` (load-shed). `target` is the
  *soft* limit driving scaling.
- **Hung-but-ready container.** `liveness` runs directly against the container (kubelet restarts on
  failure); `startup` grants slow-boot grace. Readiness alone can't recover a wedged process.
- **Eviction / node disruption.** Because the cold-flip is keyed to *zero ready endpoints*, an evicted
  or crashed last replica auto-flips to the deployments-activator → cold-start latency blip, not downtime. See
  [resource-model: Disruption & surge](resource-model.md#disruption--surge).
- **Drain.** the deployments-sidecar `preStop` finishes in-flight before the server stops;
  `terminationGracePeriodSeconds` covers the longest request, else SIGKILL drops the remainder.
- **Async callback failure.** The dispatcher retries with backoff and circuit-breaks per host;
  callbacks that exhaust retries go to an optional **`deadLetterSink`** on the `Callback` rather than
  being silently dropped.
- **Process loss — async is at-most-once.** Pending async work (a deployment `.response` in flight, a
  pool `.result` awaiting dispatch) lives only in the handling replica's memory; a crash drops it.
  That is the price of statelessness, stated plainly: callers needing certainty correlate on
  `X-Invocation-Id` / activation `id` and apply their own timeout.
- **Flip-reconciler outage.** The cold endpoint flip is reconciled by the leader-elected adapter;
  while no leader runs, a crashed last replica leaves stale endpoints and `502`s can escape. Leader
  failover time bounds that window — the flip reconciler is a data-path-correctness component and is
  sized/monitored as one.

**State mapping:** `pending` (deploying, within deadline) → `ready` (≥1 ready endpoint) → `degraded`
(some replicas unhealthy) / `failed` (deadline exceeded or crashloop) → `deleting`.

## Pools-specific

- **Activation never becomes ready** (HTTP mode — never binds `Port`): past the readiness deadline →
  `failed`, pod discarded and replenished.
- **Exec non-zero exit** — not an infra failure: `exited` with `ExitCode`/`Output` via the `.result`
  callback.
- **Claim race** — the losing replica gets `409` and retries the next free pod.
- **Claimed-but-unlabeled pod** (service crashed mid-claim) — reconcile discards it after a short TTL
  and replenishment replaces it; orphans are never resold. See [pools](pools.md#claim--replenishment).
- **Burst beyond `size`** — the configured fallback (cold-create or `429`), always logged.
- **Liveness** — restarts the bound workload in place (same materialized workspace), not a fresh claim.
