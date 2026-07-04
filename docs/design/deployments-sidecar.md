# Deployments Sidecar

The dedicated `deployments-sidecar` binary, run as a long-running native sidecar (K8s 1.29+) in every
workload pod — the **queue-proxy equivalent**. Traffic reaches the user container *only* through it
(`Service → deployments-sidecar → localhost:Port`). It owns the invariants nothing off-pod can
enforce correctly, because they're pod-local and a cache-backed remote view always lags.

It is **async-agnostic** — always a plain synchronous localhost proxy; the `202`/callback split lives
in the [deployments-activator](deployments-activator.md).

## Responsibilities

- **Readiness gating** — the pod's `readinessProbe` targets the sidecar, which runs the user's
  **readiness** probe against the container and reports combined readiness. A pod joins its revision's
  `EndpointSlice` only when the sidecar is ready, so routed traffic never hits an unready container.
  (Docker: it polls the container and refuses traffic until healthy.)
- **Faster cold start (best-effort)** — probing the container on localhost, it flips its own health
  endpoint close to the instant the server binds. On its own that buys nothing off-pod — pod
  *readiness* still propagates through the kubelet's whole-second probe and the API server — so the
  fast path is the [deployments-activator](deployments-activator.md) **probing this health endpoint
  directly** and releasing on first success; kubelet-driven endpoint readiness remains the correctness
  gate for routed traffic. A latency optimization, not a guarantee — the activator's buffer + its
  membership in the revision's endpoint set during the cold/draining window are what keep it correct
  under propagation lag.
- **Graceful drain** — on SIGTERM (a `preStop` hook): (1) fail readiness (leave the `EndpointSlice`),
  (2) sleep briefly so routing de-registers, (3) drain in-flight, (4) exit.
  `terminationGracePeriodSeconds` = `min(timeoutSeconds, maxDrainSeconds)` (operator config —
  five-minute default request timeouts must not mean five-minute drains stalling every eviction and
  Karpenter consolidation); requests still in-flight at grace expiry are SIGKILLed and dropped, so
  either the grace covers the longest request or the operator has chosen to drop the tail.
- **Concurrency metrics** — since warm traffic is off-path for our service, the sidecar is the
  metering point: it reports per-pod in-flight concurrency for the
  [deployments-autoscaler](deployments-autoscaler.md) to scrape, and enforces the optional hard
  per-pod `concurrency` cap + bounded pending queue (both full → `503` load-shed).

## Probes — readiness vs liveness vs startup

Only **readiness** is sidecar-mediated, and **only readiness honors the millisecond granularity** of
the `Probe` type — the sidecar runs it sub-second. **`liveness`** and **`startup`** are probed by the
**kubelet directly** against the user container (liveness restarts a wedged-but-ready process; startup
gives slow boots grace), so they obey Kubernetes probe semantics: **whole-second granularity, 1s
minimum** — sub-second values round up. See the [`Probes` type](deployments-service.md#domain-model).

## Shim (pools)

A dedicated pool-shim binary runs as the warm-pod entrypoint: it blocks on a workspace FIFO, then
`exec`s the activation command (replacing PID 1, so container exit == workload exit). In a warm pod,
the deployments-sidecar is also the **activation surface** — see [pools](pools.md).
