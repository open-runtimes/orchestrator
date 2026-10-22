#!/usr/bin/env bash
# Install the node mount agent (DaemonSet + CSIDriver) into the kind cluster for
# the agent-mode mount tests, from the locally built ko.local/mount-agent image.
set -euo pipefail

CONTEXT="${KUBE_CONTEXT:-kind-orchestrator-dev}"
NAMESPACE="${NAMESPACE:-orchestrator-integration}"
HELM="${HELM:-helm}"

kubectl --context "$CONTEXT" create namespace "$NAMESPACE" --dry-run=client -o yaml \
  | kubectl --context "$CONTEXT" apply -f -
"$HELM" template orchestrator charts/orchestrator \
  --namespace "$NAMESPACE" \
  --set mountAgent.enabled=true \
  --set jobs.enabled=true --set deployments.enabled=true \
  --set mountAgent.image.ref=ko.local/mount-agent:latest \
  --set mountAgent.image.pullPolicy=Never \
  --show-only templates/mount-agent.yaml \
  | kubectl --context "$CONTEXT" apply -f -
kubectl --context "$CONTEXT" -n "$NAMESPACE" rollout status daemonset/orchestrator-mount-agent --timeout=120s
