#!/usr/bin/env bash
# Install pinned dev tools into ./bin/.
# Idempotent: skips any tool already present at the pinned version.

set -euo pipefail

GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.11.4}"
KO_VERSION="${KO_VERSION:-v0.18.1}"
HELM_VERSION="${HELM_VERSION:-v3.16.3}"

TOOLS_BIN="${TOOLS_BIN:-$(pwd)/bin}"
mkdir -p "${TOOLS_BIN}"

# --- golangci-lint ---
if [[ -x "${TOOLS_BIN}/golangci-lint" ]] && "${TOOLS_BIN}/golangci-lint" version 2>/dev/null | grep -q "${GOLANGCI_LINT_VERSION#v}"; then
  echo "golangci-lint ${GOLANGCI_LINT_VERSION} already installed"
else
  echo "Installing golangci-lint ${GOLANGCI_LINT_VERSION}"
  GOBIN="${TOOLS_BIN}" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"
fi

# --- ko ---
if [[ -x "${TOOLS_BIN}/ko" ]] && "${TOOLS_BIN}/ko" version 2>/dev/null | grep -q "${KO_VERSION#v}"; then
  echo "ko ${KO_VERSION} already installed"
else
  echo "Installing ko ${KO_VERSION}"
  GOBIN="${TOOLS_BIN}" go install "github.com/google/ko@${KO_VERSION}"
fi

# --- helm ---
if [[ -x "${TOOLS_BIN}/helm" ]] && "${TOOLS_BIN}/helm" version --short 2>/dev/null | grep -q "${HELM_VERSION}"; then
  echo "helm ${HELM_VERSION} already installed"
else
  echo "Installing helm ${HELM_VERSION}"
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "${arch}" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
  esac
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT
  curl -sSfL "https://get.helm.sh/helm-${HELM_VERSION}-${os}-${arch}.tar.gz" | tar -xz -C "${tmp}"
  mv "${tmp}/${os}-${arch}/helm" "${TOOLS_BIN}/helm"
  chmod +x "${TOOLS_BIN}/helm"
fi

echo "Tools installed in ${TOOLS_BIN}"
