#!/usr/bin/env bash
# CLI contract test: proves agent-base's emitted openclaw argv against the
# REAL OpenClaw CLI inside the built image — the shim is not consulted for
# any verdict (roadmap N4; the shim-only smoke let the --type remote bug
# ship). Three stages:
#   A) boot the image with the shim on PATH purely to CAPTURE the argv the
#      entrypoint actually emits for the canary spec
#   B) cross-check every captured flag against `openclaw <cmd> --help`
#      run with the real CLI (flag drift fails here)
#   C) boot the image with NO shim and the real CLI: full first boot +
#      reconcile must exit 0 with zero [agent-entry] [warn] lines, and
#      `mcp list --json` must contain both canary servers
# Usage: scripts/contract-test.sh [IMAGE]   (default: builds nothing,
# expects the image tag given or ghcr.io/tankdonut/agent-base:contract)
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
ENGINE=${CONTRACT_ENGINE:-$(command -v podman || command -v docker)}
IMAGE=${1:-ghcr.io/tankdonut/agent-base:contract}
WORK=$(mktemp -d)
VOLUME="contract-data-$$"
trap '$ENGINE volume rm -f "$VOLUME" >/dev/null 2>&1; rm -rf "$WORK"' EXIT

pass() { echo "  PASS $1"; }
fail() { echo "  FAIL $1" >&2; exit 1; }
# shellcheck disable=SC2034
CANARY_ENV=(
  -e "ZAI_API_KEY=contract-dummy-zai"
  -e "CONTRACT_REMOTE_TOKEN=contract-dummy-bearer"
  -e "HOME=/home/node"
)
CANARY_MOUNTS=(
  --security-opt label=disable
  -v "$REPO_ROOT/scripts/contract/spec.json:/opt/agent/spec.json:ro"
  -v "$REPO_ROOT/scripts/contract/automations:/opt/agent/automations:ro"
)

echo "[contract] image: $IMAGE (engine: $ENGINE)"

# 0) loader sanity — fail fast on spec errors before any slow boot.
if "$ENGINE" run --rm "${CANARY_MOUNTS[@]}" "${CANARY_ENV[@]}" "$IMAGE" --validate-spec \
    >"$WORK/validate.log" 2>&1; then
  pass "--validate-spec accepted canary spec"
else
  fail "canary spec rejected:"$'\n'"$(sed 's/^/    /' "$WORK/validate.log")"
fi

# --- Stage A: capture emitted argv via shim (shim is recorder, not judge) ---
# 666: the container writes as mapped uid 1000, not the host user — without
# it the bind-mounted log stays empty and stage B passes vacuously.
touch "$WORK/shim.log"
chmod 666 "$WORK/shim.log"
if timeout 120 "$ENGINE" run --rm \
  -v "$REPO_ROOT/scripts/shim:/shim:ro" -v "$WORK/shim.log:/tmp/shim.log" \
  -e "OPENCLAW_SHIM_LOG=/tmp/shim.log" \
  -e "PATH=/shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  "${CANARY_MOUNTS[@]}" "${CANARY_ENV[@]}" "$IMAGE" sleep 3 >"$WORK/stage-a.log" 2>&1; then
  pass "stage A: shim boot exited 0"
else
  fail "stage A boot failed:"$'\n'"$(sed 's/^/    /' "$WORK/stage-a.log")"
fi

# --- Stage B: every emitted flag must appear in real `--help` output ---
# Subcommand paths agent-base invokes (longest-prefix match against the
# shim log). `gateway` is the supervised CMD, not a CLI call — excluded.
PATHS=(
  "setup"
  "models fallbacks add"
  "channels add"
  "plugins install"
  "plugins list"
  "config set"
  "config validate"
  "mcp list"
  "mcp add"
  "health"
  "memory status"
  "memory index"
  "doctor"
  "security audit"
  "cron list"
  "cron add"
  "cron edit"
  "cron delete"
)
help_cache_dir="$WORK/help"
mkdir -p "$help_cache_dir"
for path in "${PATHS[@]}"; do
  file="$help_cache_dir/${path// /_}.txt"
  timeout 60 "$ENGINE" run --rm --entrypoint openclaw \
    "${CANARY_ENV[@]}" "$IMAGE" $path --help >"$file" 2>&1 || true
done

violations=0
while IFS= read -r line; do
  [ -n "$line" ] || continue
  argv=$(printf '%s' "$line" | sed "s/^cwd=[^ ]* //")
  mapfile -t tokens < <(printf '%s\n' "$argv" | grep -o "'[^']*'" | tr -d "'")
  matched=""
  for path in "${PATHS[@]}"; do
    read -ra words <<<"$path"
    n=${#words[@]}
    [ "$n" -gt "${#tokens[@]}" ] && continue
    ok=1
    for ((i = 0; i < n; i++)); do
      if [ "${tokens[i]}" != "${words[i]}" ]; then
        ok=0
        break
      fi
    done
    if [ "$ok" = 1 ]; then
      matched=$path
      break
    fi
  done
  [ -n "$matched" ] || continue
  for token in "${tokens[@]}"; do
    case "$token" in
      --*) ;;
      *) continue ;;
    esac
    help_file="$help_cache_dir/${matched// /_}.txt"
    if ! grep -qE "(^|[[:space:],])${token//./\\.}\b" "$help_file"; then
      echo "  FAIL flag '${token}' (from: ${matched}) missing from '${matched} --help'" >&2
      violations=$((violations + 1))
    fi
  done
done <"$WORK/shim.log"
if [ "$violations" -eq 0 ]; then
  pass "stage B: every emitted flag exists in real CLI help"
else
  fail "stage B: $violations flag(s) drifted from the real CLI"
fi

# --- Stage C: real boot, no shim — zero warnings + registered servers ---
if timeout 420 "$ENGINE" run --rm \
  -v "$VOLUME:/home/node/.openclaw" \
  "${CANARY_MOUNTS[@]}" "${CANARY_ENV[@]}" "$IMAGE" sleep 5 >"$WORK/stage-c.log" 2>&1; then  pass "stage C: real boot exited 0"
else
  fail "stage C real boot failed:"$'\n'"$(sed 's/^/    /' "$WORK/stage-c.log")"
fi
if grep -q '\[agent-entry\] \[warn\]' "$WORK/stage-c.log"; then
  fail "stage C: boot log carries [warn] lines:"$'\n'"$(grep '\[agent-entry\] \[warn\]' "$WORK/stage-c.log" | sed 's/^/    /')"
fi
pass "stage C: zero [warn] lines in boot log"

mcp_list=$(timeout 60 "$ENGINE" run --rm --entrypoint openclaw \
  -v "$VOLUME:/home/node/.openclaw" "${CANARY_ENV[@]}" "$IMAGE" mcp list --json 2>&1) \
  || fail "mcp list --json failed against booted volume:"$'\n'"$mcp_list"
for needle in '"contract-remote"' 'mcp.invalid' '"contract-local"'; do
  if grep -qF -- "$needle" <<<"$mcp_list"; then
    pass "stage C: mcp list contains $needle"
  else
    fail "stage C: mcp list missing $needle:"$'\n'"$mcp_list"
  fi
done

echo "[contract] PASS (argv matches real CLI; real boot clean)"
