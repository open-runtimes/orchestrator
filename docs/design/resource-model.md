# Resource Model & Scheduling

Cross-cutting; applies to both services. Knative is the contrast: it sets **no** default
requests/limits (user containers end up BestEffort) and does **no** node-level work — it emits a
replica count and leaves packing, consolidation, and headroom to kube-scheduler + Karpenter. We borrow
its pod-layer primitives but, owning the platform, take on node compaction.

## Requests, limits, QoS

The user declares the **limit** (ceiling / billing unit — the spec's `cpu`/`memory`); the platform
derives the **request** via operator-configured overcommit ratios:

- **Memory** request ≈ limit — incompressible, so over-committing risks node OOM. Effectively
  Guaranteed for memory.
- **CPU** request = a fraction of the limit (compressible → safe to overcommit), leaning toward **no
  CPU limit**: CFS-quota throttling at the limit is a tail-latency killer; CPU *requests* (shares)
  handle fairness.

Result: **Burstable, memory-protected** — dense packing on small requests, bursting into idle
headroom, without OOM roulette. A `LimitRange` per shard namespace forbids BestEffort and sets sane
defaults; `ResourceQuota` caps per-tenant totals. The [deployments-sidecar](deployments-sidecar.md)/init **overhead**
(small by default, optional Knative-style percentage-of-workload sizing with min/max caps) plus
`RuntimeClass.overhead` for gvisor/kata are counted into the pod request.

## Compaction (the part Knative punts)

- **Bin-pack at schedule time** — `NodeResourcesFit` scoring set to `MostAllocated` /
  `RequestedToCapacityRatio`, so pods fill nodes and leave whole nodes empty to remove.
- **Consolidation** — Karpenter repacks underutilized nodes onto fewer and deletes empties (the main
  "stay compact" engine; also right-sizes instance types).
- **Descheduler** (`LowNodeUtilization`) — re-pack the fragmentation that scale-to-zero churn leaves.

## Balance & availability

Pack **across** deployments, spread **within** one: `topologySpreadConstraints`/anti-affinity on a
deployment's own replicas so a node loss can't take all replicas of one service. Unlike Knative (no
per-revision PDB → workload pods freely evicted on drain), we attach a **replica-aware
PodDisruptionBudget** and rely on deployments-sidecar graceful drain.

## Disruption & surge

Voluntary disruptions (Karpenter consolidation, node drain/upgrade, descheduler) go through the
eviction API, which is **remove-only** — there is no native surge-before-terminate (`maxSurge` is
rollout-only). For a single-replica deployment that's the classic PDB bind: `maxUnavailable: 0`
**deadlocks** the node autoscaler, `maxUnavailable: 1` **dips** capacity. Resolved in layers, cheapest
first:

1. **Multi-replica + headroom** — run one spare, PDB `minAvailable = desired`; eviction takes the
   spare, Service LB hides it. No dip, no surge — and it covers node *failure* too.
2. **Deployments-activator buffers the gap** (the default, the Knative move) — because the cold-flip is keyed to
   *zero ready endpoints*, an evicted/crashed last replica auto-flips to the [deployments-activator](deployments-activator.md),
   which buffers until the replacement is Ready. **Lossless**, at the cost of a cold-start latency blip
   — so for single-replica, eviction is a *latency* event, not downtime. Nearly free (we already run
   the deployments-activator).
3. **Surge** (opt-in latency optimization) — to remove even that blip: PDB `minAvailable: 1` + a
   controller that scales **+1 on a blocked eviction**, lets the PDB admit the retried eviction once
   the surge pod is Ready, then scales back (true add-before-remove, exploiting the eviction API's
   retry). A **stand-in for KEP-4563 (EvictionRequest API, alpha in k8s 1.37)**, which makes the
   workload controller a participant in eviction so it can recreate-before-terminate natively — delete
   our surge controller when that GAs.

PDB policy is replica-aware (no/permissive PDB at 1 replica unless surging), with
`unhealthyPodEvictionPolicy: AlwaysAllow` so broken pods never deadlock a drain.

## Cold-start vs compaction

Tight packing means a scale-from-zero with no spare capacity waits for Karpenter to provision a node.
Reconcile with **low-`PriorityClass` headroom / "balloon" pods** that reserve *reclaimable* capacity,
**preempted** the instant a real pod needs it — instant cold-start scheduling *and* compaction.
**Warm-pool pods are not headroom**: they are paid-for standing capacity and run at normal priority —
preempting one would convert a deployment cold start into a pool miss, trading the one guarantee pools
exist to make. Borrow Knative's `scale-down-delay` / pod-retention to damp scale-to-zero churn so it
doesn't thrash nodes against consolidation.

## Per-runtime node pools

runc/gvisor pack on general nodes; **kata** lands on a nested-virt/bare-metal pool (its VM
`RuntimeClass.overhead` is real memory → far fewer pods/node), placed automatically by
`RuntimeClass.scheduling`. Each pool bin-packs independently. See [security: sandbox tier](security.md#workload-hardening).

## Scale & limits

Two ceilings, decoupled by scale-to-zero (an idle deployment ≈ 12 objects at the default
`revisionHistoryLimit: 3` — marker + `HTTPRoute` + ~3 objects per retained revision — 0 pods):

| Limit | Bounds | Rough ceiling (single tuned cluster) |
|-------|--------|--------------------------------------|
| Gateway config (`HTTPRoute` → routes) | total registered | ~10k comfortable, degrades toward ~50k |
| Per-revision Services / `EndpointSlice`s | total registered | endpoint-routing gateway → ~30k+ |
| etcd object count | total registered | ~300k objects / ~12 ≈ ~25k deployments |
| Deployments-autoscaler metric scrape fan-out | **concurrently warm** | ~1k–5k warm revisions per leader |
| Pods / nodes / IPs | concurrently warm | 150k-pod tested envelope |

**Headline:** order **10k–25k registered** (idle, scale-to-zero), **~1k–5k concurrently warm per
control-plane shard**. First walls are gateway config size and the deployments-autoscaler scrape — lifted by
**namespace + control-plane sharding** (see [security: namespace model](security.md#namespace-model)),
endpoint-routing gateways, aggressive **revision GC**, and push metrics. Docker is single-host:
dozens–low hundreds, dev-scale.
