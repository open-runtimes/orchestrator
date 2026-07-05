# Gateway API PoC — Traefik on kind

Proves the two load-bearing assumptions in `docs/design/gateway-routing.md`:

- **Revision identity** — per-backendRef `RequestHeaderModifier` (`set: X-Revision`) on
  weighted `backendRefs` (Gateway API **Extended**), with `set` overwriting any
  client-supplied `X-Revision`.
- **Async routing** — a second rule with an **Exact** header match on
  `Prefer: respond-async`, sorted ahead of the default rule by Gateway API precedence
  regardless of listed order (the manifest deliberately lists the default rule first).

## Versions

| Component | Version |
|---|---|
| kind node | kindest/node:v1.30.4 (cluster `orchestrator-dev`, context `kind-orchestrator-dev`) |
| Gateway API CRDs | v1.5.1 standard channel |
| Traefik chart | 41.0.1 |
| Traefik app | v3.7.5 |

## Install

```sh
task kind:up

# Gateway API v1.5.1 standard CRDs.
# Note: on K8s 1.30 the TLSRoute CRD fails to apply — its CEL rule uses isIP(),
# available only on K8s >= 1.31. TLSRoute is unused here; we re-applied it with
# that one validation rule stripped so Traefik's provider finds the CRD.
kubectl --context kind-orchestrator-dev apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml

helm repo add traefik https://traefik.github.io/charts
helm install traefik traefik/traefik --version 41.0.1 \
  --kube-context kind-orchestrator-dev \
  --namespace traefik-system --create-namespace \
  -f hack/gateway-poc/traefik-values.yaml

kubectl --context kind-orchestrator-dev apply -f hack/gateway-poc/
```

Traefik quirk: a Gateway listener is only Accepted if its port matches an
**entrypoint** port (otherwise `PortUnavailable: no matching entryPoint for port 80`).
`traefik-values.yaml` moves the `web` entrypoint from 8000 to 80 and sets the
kubelet-safe `net.ipv4.ip_unprivileged_port_start=0` sysctl so the non-root pod can
bind it.

## Verify

From an in-cluster curl pod, against the Traefik ClusterIP Service:

```sh
kubectl --context kind-orchestrator-dev -n gwtest run curl --image=curlimages/curl --restart=Never -- sleep infinity

# Weighted split + revision identity (whoami echoes received headers):
kubectl --context kind-orchestrator-dev -n gwtest exec curl -- sh -c \
  'i=0; while [ $i -lt 200 ]; do curl -s -H "Host: demo.test" http://traefik.traefik-system/ | grep ^X-Revision; i=$((i+1)); done' | sort | uniq -c

# Forged client header is overwritten (set, not add):
kubectl --context kind-orchestrator-dev -n gwtest exec curl -- \
  curl -s -H "Host: demo.test" -H "X-Revision: forged" http://traefik.traefik-system/

# Async rule (exact match):
kubectl --context kind-orchestrator-dev -n gwtest exec curl -- \
  curl -s -H "Host: demo.test" -H "Prefer: respond-async" http://traefik.traefik-system/

# Combined RFC 7240 form — NOT recognized by the Exact match, hits the default rule:
kubectl --context kind-orchestrator-dev -n gwtest exec curl -- \
  curl -s -H "Host: demo.test" -H "Prefer: respond-async, wait=100" http://traefik.traefik-system/
```

## Observed results (2026-07-05)

| Check | Result |
|---|---|
| 90/10 weighted split, 200 reqs | **180 / 20** (exactly 90%/10% — Traefik smooth WRR is deterministic) |
| `X-Revision` matches serving pod (`Hostname:` prefix), 200 reqs | **0 mismatches** |
| Client sends `X-Revision: forged` | Backend sees gateway value (`rev-a`), never `forged` |
| `Prefer: respond-async`, 20 reqs | **20/20** on rev-async pods with `X-Revision: rev-async` |
| `Prefer: respond-async, wait=100`, 10 reqs | **10/10** hit the DEFAULT rule (9 rev-a / 1 rev-b) — combined list form not matched, as the design documents |
| Rule precedence (default rule listed FIRST in spec) | Header-match rule still wins for exact `respond-async` — Gateway API precedence sorting confirmed |

Status conditions at pass: Gateway `Accepted=True, Programmed=True`; listener
`Accepted/ResolvedRefs/Programmed=True`; HTTPRoute `Accepted=True, ResolvedRefs=True`.
