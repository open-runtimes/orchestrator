# Deployments Autoscaler

A concurrency-only autoscaler — a tiny KPA. A single leader-elected goroutine (the lease the K8s job
backend already uses). It **ticks fast, smooths slow** — every tick (default 2s) it recomputes over a
sliding stable window (default 60s) of observations; the window is a smoothing horizon, **not** the
reaction time:

```
desired = clamp(ceil(avgConcurrencyOverWindow / autoscaling.target),
                autoscaling.minReplicas, autoscaling.maxReplicas)
```

It patches the revision Deployment's `spec.replicas` via the `scale` subresource.

**Ownership split with the activator.** The [deployments-activator](deployments-activator.md) owns
`0→1`: on the first cold hit it raises the revision itself (a patch of the same `scale` subresource —
its only write; `Scale(_, 1)` on Docker), never waiting for a tick. The autoscaler owns `1↔N` and
`N→0`. Both writes are idempotent clamps, so they can't fight.

## Metric source

- **Warm**: `observedConcurrency` is aggregated from the revision's
  [deployments-sidecar](deployments-sidecar.md) metrics (scraped). Because warm traffic is off our
  service's path, the sidecar — not a central counter — is the metering point (exactly Knative's
  reason for queue-proxy metrics).
- **At zero**: `0→1` is not scrape-driven — the activator raises it directly (above). The activator's
  queued count is scraped only to *hold* the revision up and scale past 1 while requests are still
  buffered.

## Scale-to-zero

When `autoscaling.minReplicas: 0`, the deployment scales to zero after concurrency stays 0 for the
full window; the routable Service's endpoints then flip to the activator (driven by ready-endpoint
count — the [cold endpoint flip](deployments-activator.md#cold-endpoint-flip-the-sks-mechanism)) so the
next request cold-starts it. v1 is **concurrency-only** — no RPS mode, no panic window; the fast tick
plus activator-owned `0→1` bound burst reaction to seconds (a panic window would only sharpen `1↔N`
under spikes — add it later if shedding shows up in practice).

## Bounds & scope

`autoscaling.target` is the *soft* limit that drives scaling; the optional `concurrency` field is the
*hard* per-pod ceiling enforced in the deployments-sidecar. On **Docker** the loop is clamped to
`maxReplicas: 1` — only the `0↔1` transitions; `1↔N` is K8s-only.

## Relationship to node scaling

The autoscaler emits a **replica count only** — node provisioning/packing is delegated to
kube-scheduler + Karpenter (see [resource-model](resource-model.md)). The scrape fan-out across many
revisions is the practical ceiling on concurrently-warm deployments; lift it by sharding the
autoscaler across [namespace shards](security.md#namespace-model) (each leader owns a subset) or
moving to push metrics.
