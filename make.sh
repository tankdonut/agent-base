#!/bin/sh
# Task runner for the agent-base repo — mirrors the commands table in
# AGENTS.md. Container engine is podman when available, docker otherwise.
set -eu

REPO_ROOT=$(cd "$(dirname "$0")" && pwd)
cd "$REPO_ROOT"

IMAGE=ghcr.io/tankdonut/agent-base

engine() {
  if command -v podman >/dev/null 2>&1; then
    printf '%s\n' podman
  elif command -v docker >/dev/null 2>&1; then
    printf '%s\n' docker
  else
    echo "error: podman or docker is required for this target" >&2
    exit 1
  fi
}

usage() {
  cat <<EOF
Usage: ./make.sh <target>

Targets:
  test    Run container module tests (python3 -m unittest discover container)
  lint    Run pre-commit on all files
  smoke   Build the smoke image: fixture boots + graceful-shutdown drain
          (scripts/smoke.sh; SMOKE_ENGINE overrides engine detection)
  build   Build $IMAGE:<AGENT_BASE_VERSION or today's date>
  push    Push $IMAGE:\$AGENT_BASE_VERSION (env var must be set explicitly)
  help    Show this help

Environment:
  AGENT_BASE_VERSION  Tag to build/push (default for build: \$(date +%Y.%m.%d);
                      required for push — an accidental date-tag push is refused)
EOF
}

target=${1:-help}

case $target in
  test)
    python3 -m unittest discover -s container
    ;;
  lint)
    pre-commit run --all-files
    ;;
  smoke)
    scripts/smoke.sh
    ;;
  build)
    version=${AGENT_BASE_VERSION:-$(date +%Y.%m.%d)}
    eng=$(engine)
    echo "[make] building $IMAGE:$version with $eng"
    format_flag=""
    if [ "$eng" = podman ]; then
      # podman defaults to OCI, which cannot carry the Dockerfile
      # HEALTHCHECK into the artifact.
      format_flag="--format docker"
    fi
    "$eng" build $format_flag -f container/Dockerfile \
      -t "$IMAGE:$version" \
      --build-arg AGENT_BASE_VERSION="$version" .
    ;;
  push)
    # Pushing the implicit date tag would race a CI build of the same day;
    # require an explicit version instead.
    version=${AGENT_BASE_VERSION:?set AGENT_BASE_VERSION=YYYY.MM.DD[.N] to push}
    eng=$(engine)
    "$eng" push "$IMAGE:$version"
    ;;
  help | -h | --help)
    usage
    ;;
  *)
    echo "error: unknown target '$target'" >&2
    usage >&2
    exit 1
    ;;
esac
