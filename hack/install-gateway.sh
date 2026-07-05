#!/usr/bin/env bash
# Install Gateway API CRDs + Traefik into the kind dev cluster.
# Versions proven by hack/gateway-poc/ (see its README for the verification).
set -euo pipefail

CONTEXT="${KUBE_CONTEXT:-kind-orchestrator-dev}"
GATEWAY_API_VERSION="v1.5.1"
TRAEFIK_CHART_VERSION="41.0.1"

kubectl --context "$CONTEXT" apply -f \
  "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"

helm repo add traefik https://traefik.github.io/charts >/dev/null
helm repo update traefik >/dev/null
if ! helm upgrade --install traefik traefik/traefik \
  --kube-context "$CONTEXT" \
  --namespace traefik-system --create-namespace \
  --version "$TRAEFIK_CHART_VERSION" \
  --values "$(dirname "$0")/gateway-poc/traefik-values.yaml" \
  --wait --timeout 5m; then
  echo "--- traefik install failed; dumping state ---" >&2
  kubectl --context "$CONTEXT" -n traefik-system get pods -o wide >&2 || true
  kubectl --context "$CONTEXT" -n traefik-system describe pods >&2 || true
  kubectl --context "$CONTEXT" -n traefik-system get events --sort-by=.lastTimestamp >&2 | tail -20 || true
  exit 1
fi

echo "Gateway API ${GATEWAY_API_VERSION} + Traefik chart ${TRAEFIK_CHART_VERSION} installed."
