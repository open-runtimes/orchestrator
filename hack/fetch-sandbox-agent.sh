#!/usr/bin/env bash
# Fetch the pinned open-runtimes/sandbox agent binaries into the pool-shim's
# kodata, so the shim-install init container can copy one into a sandbox's
# workspace at pod creation.
#
# Vendored at image-build time on purpose: a warm pod is created for every
# sandbox that will ever be claimed, and downloading the agent per pod would put
# a GitHub fetch (and its rate limits and outages) on the sandbox creation path.
# The binaries are static, so one per architecture serves every base image
# (verified against glibc, musl, and distroless).
#
# Both are placed in the image because ko's kodata is shared across platforms;
# the shim picks the one matching its own GOARCH at install time.
#
# THE PIN: version and digests below are the whole contract with upstream. The
# digests are deliberately NOT read from the release's own checksums.txt — that
# file can be replaced along with the asset it describes, so it proves the
# download survived transit and nothing more. Committing them here means a
# re-cut release fails the build instead of silently changing what runs inside
# every sandbox. To bump: change VERSION and both digests in one commit, so the
# diff shows exactly which bytes were reviewed.
set -euo pipefail

VERSION="v0.1.0"
SHA256_amd64="0d6d8d2a39b2e0a13be2b085491e40747d9f1b03e04bc7e81ebf1e2131a6a925"
SHA256_arm64="00f6159561c3990dc9d0d2ed34a234bb95a78f6ec19ee1eb6ba4a1f4e4f76ce6"

DEST="$(dirname "$0")/../cmd/pool-shim/kodata"
BASE="https://github.com/open-runtimes/sandbox/releases/download/${VERSION}"

mkdir -p "$DEST"

for arch in amd64 arm64; do
  expected="SHA256_${arch}"
  expected="${!expected}"
  out="${DEST}/agent-linux-${arch}"
  stamp="${out}.version"

  # The stamp carries the digest as well as the version, so editing the pin
  # re-fetches even if the version string is unchanged.
  if [[ -f "$out" && -f "$stamp" && "$(cat "$stamp")" == "${VERSION} ${expected}" ]]; then
    echo "sandbox agent ${VERSION} linux/${arch} already vendored"
    continue
  fi

  asset="sandbox_${VERSION}_linux_${arch}.tar.gz"
  work="$(mktemp -d)"
  echo "Fetching sandbox agent ${VERSION} linux/${arch}"
  curl -fsSL -o "${work}/${asset}" "${BASE}/${asset}"

  got="$(shasum -a 256 "${work}/${asset}" | awk '{print $1}')"
  if [[ "$expected" != "$got" ]]; then
    echo "PIN MISMATCH for ${asset}" >&2
    echo "  expected ${expected} (hack/fetch-sandbox-agent.sh)" >&2
    echo "  got      ${got}" >&2
    echo "Upstream re-cut this release, or the download was tampered with. Verify" >&2
    echo "the new artifact before updating the pin." >&2
    rm -rf "$work"
    exit 1
  fi

  tar -xzf "${work}/${asset}" -C "$work" sandbox
  install -m 0755 "${work}/sandbox" "$out"
  echo "${VERSION} ${expected}" > "$stamp"
  rm -rf "$work"
done

echo "Sandbox agent ${VERSION} vendored into ${DEST}"
