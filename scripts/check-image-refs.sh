#!/usr/bin/env bash
# Release-time image-ref gate (roadmap N5): every agent-base image tag
# referenced by a TRACKED file must exist in the GHCR registry. A ref to a
# never-published tag (the 2026.08.21 class: docs written against a local
# build) fails the release instead of shipping a broken quick-start.
#
# Queries the GHCR v2 API directly (curl + anonymous token): engine-neutral
# and works for both single manifests and manifest lists — podman's
# `manifest inspect` rejects single-image manifests, docker's is
# docker-only, and this must run identically on CI runners and dev hosts.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
IMAGE=ghcr.io/tankdonut/agent-base
cd "$REPO_ROOT"

# Tracked files only — .omo/ and other ignored artifacts never gate releases.
mapfile -t refs < <(
  git grep -hoE "${IMAGE//./\\.}:202[0-9]\.[0-9]{2}\.[0-9]{2}" -- . |
    sed "s|^${IMAGE//./\\.}:||" | sort -u
)

if [ "${#refs[@]}" -eq 0 ]; then
  echo "[check-image-refs] no tracked ${IMAGE}:<date> refs found (nothing to verify)"
  exit 0
fi

token=$(
  curl -sf "https://ghcr.io/token?scope=repository:tankdonut/agent-base:pull&service=ghcr.io" |
    sed -n 's/.*"token":"\([^"]*\)".*/\1/p'
)
if [ -z "$token" ]; then
  echo "[check-image-refs] could not obtain anonymous GHCR token" >&2
  exit 1
fi

echo "[check-image-refs] verifying ${#refs[@]} tag(s): ${refs[*]}"
missing=0
for tag in "${refs[@]}"; do
  if curl -sf \
    -H "Authorization: Bearer $token" \
    -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json" \
    "https://ghcr.io/v2/tankdonut/agent-base/manifests/$tag" >/dev/null; then
    echo "  PASS $IMAGE:$tag"
  else
    echo "  FAIL $IMAGE:$tag — referenced by a tracked file but absent from the registry" >&2
    missing=$((missing + 1))
  fi
done

if [ "$missing" -ne 0 ]; then
  echo "[check-image-refs] FAIL: $missing unpublished tag ref(s)" >&2
  exit 1
fi
echo "[check-image-refs] PASS"
