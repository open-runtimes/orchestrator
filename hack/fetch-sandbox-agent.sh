#!/usr/bin/env bash
# Fetch the pinned open-runtimes/sandbox agent binaries into the pool-shim's
# kodata, so the shim-install init container can copy one into a sandbox's
# workspace at pod creation.
#
# Vendored at image-build time on purpose: a warm pod is created for every
# sandbox that will ever be claimed, and downloading the agent per pod would put
# a GitHub fetch (and its rate limits and outages) on the sandbox creation path.
# The binaries are static, so one per architecture serves every base image.
#
# Both are placed in the image because ko's kodata is shared across platforms;
# the shim picks the one matching its own GOARCH at install time.
set -euo pipefail

VERSION="${SANDBOX_AGENT_VERSION:-v0.1.0}"
DEST="$(dirname "$0")/../cmd/pool-shim/kodata"
BASE="https://github.com/open-runtimes/sandbox/releases/download/${VERSION}"

mkdir -p "$DEST"

# The release publishes one checksums.txt covering every asset; it is the only
# thing we trust the download against.
checksums="$(mktemp)"
trap 'rm -f "$checksums"' EXIT
curl -fsSL -o "$checksums" "${BASE}/checksums.txt"

for arch in amd64 arm64; do
  out="${DEST}/agent-linux-${arch}"
  stamp="${out}.version"
  if [[ -f "$out" && -f "$stamp" && "$(cat "$stamp")" == "$VERSION" ]]; then
    echo "sandbox agent ${VERSION} linux/${arch} already vendored"
    continue
  fi

  asset="sandbox_${VERSION}_linux_${arch}.tar.gz"
  work="$(mktemp -d)"
  echo "Fetching sandbox agent ${VERSION} linux/${arch}"
  curl -fsSL -o "${work}/${asset}" "${BASE}/${asset}"

  want="$(grep "  ./${asset}\$" "$checksums" | awk '{print $1}')"
  if [[ -z "$want" ]]; then
    echo "no checksum for ${asset} in ${BASE}/checksums.txt" >&2
    exit 1
  fi
  got="$(shasum -a 256 "${work}/${asset}" | awk '{print $1}')"
  if [[ "$want" != "$got" ]]; then
    echo "checksum mismatch for ${asset}: want ${want}, got ${got}" >&2
    exit 1
  fi

  tar -xzf "${work}/${asset}" -C "$work" sandbox
  install -m 0755 "${work}/sandbox" "$out"
  echo "$VERSION" > "$stamp"
  rm -rf "$work"
done

echo "Sandbox agent ${VERSION} vendored into ${DEST}"
