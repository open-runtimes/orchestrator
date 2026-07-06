# Security & Isolation

A **public-facing** serving plane running **untrusted** tenant code (deployments and pool functions),
unlike the network-isolated jobs internal endpoint. Isolation is a first-class concern.

- **Authz to manage** — create/update/delete/traffic and pool activation go through the same API-key
  auth middleware as jobs; multi-tenant scoping (who may use which `host`) is enforced there.
- **Workload network isolation** — an optional cluster-scoped **`ClusterwideNetworkPolicy`** (Cilium
  `CiliumClusterwideNetworkPolicy` / Calico `GlobalNetworkPolicy`) denies pod-to-pod traffic except
  from the gateway/deployments-activator and restricts egress, selecting workloads **by label** so it isolates
  tenants across *all* shard namespaces. Opt-in and CNI-specific; falls back to per-shard namespaced
  `NetworkPolicy`.
- **Control surfaces are not workload-reachable** — two of our own endpoints would bypass API auth if
  open in-cluster: the **warm-pod sidecar's activation POST** (ingress restricted to the
  deployments-service *and* authenticated by a per-pod claim token — see [pools](pools.md)), and the
  **deployments-activator** (trusts the gateway-set `X-Revision`, so its ingress is gateway-only).
- **Edge** — TLS per host is the gateway's `Listener` (cert-manager). Request-level filtering
  (rate-limit/auth/WAF) is out of scope for now.
- **Secret material at rest (closed, Phase 6)** — on the Kubernetes backend nothing secret rests on
  ConfigMap/annotation surfaces anymore:
  - the **deployment spec JSON** (carries the callback HMAC key) lives on a per-deployment `Secret`
    `dep-{id}`; the marker ConfigMap keeps only lifecycle state (revisions/traffic mode/host) and
    remains the identity anchor. The service and the activator read the Secret under a narrow
    `secrets` RBAC rule.
  - **pool claim tokens are derived, never stored**: `hex(HMAC-SHA256(installKey, podName))`, with
    the install key in the `pool-claim-key` Secret (get-or-created on start). Warm pods still get
    the token as sidecar env — the same exposure class as the pod's own env — but no annotation.
  - the **pool activation-spec annotation** is written with the callback key **redacted**; the full
    callback exists only in the in-flight request. A service restart therefore cannot deliver
    callbacks for reconstructed activations — the already-documented at-most-once semantics.

  Remaining out of scope: the **Docker backends** (single-host dev, no RBAC boundary) keep spec/
  token material in labels, and per-activation callback keys are never persisted anywhere (by
  design, see above).

## Workload hardening

Every generated pod (user container **and** the deployments-sidecar/shim sidecars) runs under a hardened
`SecurityContext`, with **Pod Security Admission `restricted`** on the shard namespaces as the
cluster-side backstop. The floor (always-on; dangerous knobs non-overridable):

- `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, never `privileged`, no host namespaces.
- `capabilities.drop: ["ALL"]`, add back none (deployments-sidecar fronts the container → high port, no
  `NET_BIND_SERVICE`).
- `seccompProfile: RuntimeDefault` (a `Localhost` custom profile is the opt-in tightening).
- `readOnlyRootFilesystem: true` + a writable `emptyDir` for the workspace and `/tmp`.
- **`automountServiceAccountToken: false`** — user pods get no Kubernetes API token.
- `resources.limits` + a **PID limit** (fork-bomb guard).
- **Block the cloud metadata endpoint** (`169.254.169.254`) in the `ClusterwideNetworkPolicy` egress
  rules — the highest-value network rule (stops IAM-credential SSRF).

### Sandbox tier — `sandbox` field selects a `RuntimeClass`

| `sandbox` | Isolation | Notes |
|-----------|-----------|-------|
| `runc` (**default**) | namespaces + cgroups + the floor above | shared host kernel; fastest. **Current default — may move to `gvisor` later.** |
| `gvisor` | + user-space kernel (syscall interception) | moderate startup/overhead; strong escape resistance |
| `kata` | + micro-VM (real guest kernel) | heaviest startup/memory; max isolation; gated to nested-virt/bare-metal nodes via `RuntimeClass.scheduling` + `overhead`; the warm pool hides its boot cost |

The orchestrator stamps `spec.runtimeClassName` and validates the class is installed. **Posture
caveat:** with the `runc` default, workloads are **not** kernel-sandboxed — isolation rests on the
floor + the network policy; reach for `gvisor`/`kata` for hostile multi-tenancy. For **pools**,
`sandbox` is a `Pool` dimension (warm pods are runtime-fixed at creation), so warm fleets are keyed by
`(image, sandbox)`.

**Tension — `mount` artifacts.** squashfs `mount` needs `CAP_SYS_ADMIN` / the `mount` syscall, which
fights drop-ALL-caps and gVisor. Do the mount in the **sidecar only** with a single scoped capability,
or use rootless `squashfuse` — never grant it to the user container.

## Namespace model

Deployments and pool activations are spread across a fixed pool of `K` operator-provisioned **shard
namespaces** — assigned least-loaded at create and recorded on the deployment's marker ConfigMap (keeps the
service stateless and lets `K` grow without remapping). Sharding bounds pods-per-namespace, which caps
the per-namespace label-selector / `EndpointSlice` scan cost that makes a single giant namespace
expensive, and bounds blast radius — **without** the per-deployment-namespace costs (cluster-wide
create-namespace RBAC, a `ReferenceGrant` per deployment, unbounded namespace count). The same shards
double as **control-plane shards**: each leader-elected adapter/deployments-autoscaler owns a subset, lifting the
metric-scrape ceiling.

Sharding is a **scale** partition, not an isolation one — a shard co-locates unrelated tenants. That's
fine because isolation is enforced **orthogonally** by the label-based `ClusterwideNetworkPolicy`,
which spans shards. So scale (sharding) and isolation (cluster-wide policy) are independent knobs, and
neither requires namespace-per-tenant.
