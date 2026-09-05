#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(mktemp)"
config="$(mktemp)"
authorization="$(mktemp)"
trap 'rm -f "$rendered" "$config" "$authorization"' EXIT
printf '%s' 'Bearer test' >"$authorization"

helm template orchestrator "$repo_root/charts/orchestrator" \
  --namespace orchestrator \
  --set logCollector.enabled=true \
  --set logCollector.otlp.endpoint=http://otel-collector:4318 \
  --set logCollector.otlp.auth.existingSecret=logs-auth \
  --show-only templates/log-collector.yaml >"$rendered"

awk '
  /^  config\.alloy: \|$/ { found=1; next }
  found && /^---$/ { exit }
  found { sub(/^    /, ""); print }
  END { if (!found) exit 1 }
' "$rendered" >"$config"

docker run --rm \
  -e NODE_NAME=test-node \
  -e OTLP_ENDPOINT=http://otel-collector:4318 \
  -v "$config:/etc/alloy/config.alloy:ro" \
  -v "$authorization:/var/run/secrets/orchestrator-log-collector/authorization:ro" \
  grafana/alloy:v1.19.2 \
  validate --stability.level=public-preview /etc/alloy/config.alloy

helm template orchestrator "$repo_root/charts/orchestrator" \
  --namespace orchestrator \
  --set logCollector.enabled=true \
  --set logCollector.otlp.endpoint=http://otel-collector:4318 \
  --show-only templates/log-collector.yaml >"$rendered"

awk '
  /^  config\.alloy: \|$/ { found=1; next }
  found && /^---$/ { exit }
  found { sub(/^    /, ""); print }
  END { if (!found) exit 1 }
' "$rendered" >"$config"

docker run --rm \
  -e NODE_NAME=test-node \
  -e OTLP_ENDPOINT=http://otel-collector:4318 \
  -v "$config:/etc/alloy/config.alloy:ro" \
  grafana/alloy:v1.19.2 \
  validate --stability.level=public-preview /etc/alloy/config.alloy

if helm template orchestrator "$repo_root/charts/orchestrator" \
  --namespace orchestrator \
  --set logCollector.enabled=true >/dev/null 2>&1; then
  echo "logCollector.enabled=true unexpectedly accepted an empty OTLP endpoint" >&2
  exit 1
fi
