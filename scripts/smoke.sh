#!/usr/bin/env bash
# Image smoke test: build agent-base once, boot each fixture (freya-like,
# mimir-like) against the fake openclaw CLI (scripts/shim/openclaw), and
# assert the boot's phase order from the shim's invocation log.
#
# Two runs per fixture:
#   1. entrypoint --validate-spec — spec + automations parse, no mutation
#   2. inline phase runner — full boot (first boot + reconcile + seed +
#      post_startup) WITHOUT the fork/execvp gateway handoff (the container
#      command is overridden, so no gateway process is started)
#
# The shim log contains resolved arg values by design (that is what the
# assertions grep); it is a throwaway local artifact, removed on success.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT"

IMAGE=${AGENT_BASE_IMAGE:-${1:-agent-base:smoke}}
ENGINE=$(command -v podman >/dev/null 2>&1 && echo podman || echo docker)

FAILURES=0
pass() { printf '  PASS %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; FAILURES=$((FAILURES + 1)); }

# Mirrors entrypoint.main() minus the fork/execvp handoff. Each run starts
# a fresh container (no volume), so openclaw.json is absent and the
# first-boot path always executes. The shim log is printed after a marker
# line instead of bind-mounting the log file out (rootless uid mapping
# makes single-file mounts unreliable).
read -r -d '' RUNNER <<'EOF' || true
import os, sys
sys.path.insert(0, "/opt/agent")
import entrypoint

env = os.environ
spec = entrypoint.load_agent_spec(env)
entrypoint.data_dir().mkdir(parents=True, exist_ok=True)

if not (entrypoint.data_dir() / "openclaw.json").exists():
    entrypoint.first_boot_setup(spec)
if env.get("AGENT_MANAGE_CONFIG", "1") == "1":
    entrypoint.reconcile_config(spec, env)
    entrypoint.reconcile_mcp(spec, env)
    entrypoint.reconcile_plugins(spec)
if spec.features.gh_auth:
    entrypoint.authenticate_gh(env)
entrypoint.seed_content(spec, env)
entrypoint.post_startup(spec, env)

print("=== SHIM LOG ===")
with open(os.environ["OPENCLAW_SHIM_LOG"], encoding="utf-8") as f:
    sys.stdout.write(f.read())
EOF

assert_present() { # assert_present PATTERN DESCRIPTION
  if grep -q -- "$1" "$LOG"; then pass "$2"; else fail "$2 (pattern not found: $1)"; fi
}
first_line() { grep -n -- "$1" "$LOG" | head -n 1 | cut -d: -f1; }

smoke_fixture() { # smoke_fixture FIXTURE EXPECTED_MCP_NAME
  local f=$1 mcp=$2
  local validate_log=".smoke-$f.validate.log" boot_log=".smoke-$f.boot.log"
  LOG=".smoke-$f.shim.log"
  echo "[smoke] fixture: $f"

  # Env every spec template can resolve ({env:...} refs resolve at load
  # time even under if_env — absent vars abort the loader). Values are
  # dummies. PATH puts the shim ahead of the real CLI; HOME pins the
  # data dir to /home/node/.openclaw.
  local -a common=(
    "$ENGINE" run --rm
    # Rootless podman on this host denies the container access to
    # bind-mounted repo files under the default MCS relabel (verified for
    # the image user, --user 0:0, and --userns=keep-id); disabling the
    # label relabel fixes it. Safe here: throwaway local test container.
    --security-opt label=disable
    -e "TELEGRAM_CHAT_ID=123456"
    -e "TELEGRAM_ALLOWED_USERS=111111,222222"
    -e "TELEGRAM_TOPIC_MORNING=777"
    -e "AC_INFINITY_EMAIL=smoke@example.com"
    -e "AC_INFINITY_PASSWORD=smoke-password"
    -e "ALPHAVANTAGE_API_KEY=smoke-av-key"
    -e "LUNARCRUSH_API_KEY=smoke-lc-key"
    -e "DATABASE_URL=postgres://localhost/smoke"
    -e "AGENT_GIT_TOKEN=smoke-gh-token"
    -e "HOME=/home/node"
    -e "OPENCLAW_SHIM_LOG=/tmp/shim.log"
    -e "PATH=/shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    -v "$REPO_ROOT/scripts/shim:/shim:ro"
    -v "$REPO_ROOT/fixtures/$f/spec.json:/opt/agent/spec.json:ro"
    -v "$REPO_ROOT/fixtures/$f/automations:/opt/agent/automations:ro"
    -v "$REPO_ROOT/fixtures/$f/workspace:/opt/seed/workspace:ro"
    -v "$REPO_ROOT/fixtures/$f/docs:/opt/seed/docs:ro"
  )
  # Not every fixture ships skills (mimir-like does not); the image's empty
  # /opt/seed/skills placeholder covers it.
  if [ -d "$REPO_ROOT/fixtures/$f/skills" ]; then
    common+=(-v "$REPO_ROOT/fixtures/$f/skills:/opt/seed/skills:ro")
  fi

  # 1) --validate-spec: args after the image replace CMD, so this runs
  #    tini -- python3 /opt/agent/entrypoint.py --validate-spec.
  if "${common[@]}" "$IMAGE" --validate-spec >"$validate_log" 2>&1; then
    pass "--validate-spec accepted spec + automations"
  else
    fail "--validate-spec rejected the fixture:" && sed 's/^/    /' "$validate_log" >&2
    return 0
  fi

  # 2) full boot via the inline runner (tini bypassed; command = python3).
  if "${common[@]}" --entrypoint python3 "$IMAGE" -c "$RUNNER" >"$boot_log" 2>&1; then
    pass "full boot (first boot + reconcile + seed + post_startup) exited 0"
  else
    fail "full boot exited nonzero:" && sed 's/^/    /' "$boot_log" >&2
    return 0
  fi
  sed -n '/^=== SHIM LOG ===$/,$p' "$boot_log" | tail -n +2 >"$LOG"

  # --- shim.log assertions (args are single-quoted per arg in the log) ---
  assert_present "'setup'" "first boot: openclaw setup ran"
  assert_present "'config' 'set'" "reconcile_config applied config entries"
  assert_present "'mcp' 'add' '$mcp'" "reconcile_mcp registered '$mcp'"
  assert_present "'cron' 'list'" "post_startup seeded cron jobs (cron list)"
  assert_present "'memory' 'status'" "memory ladder checked index status"
  assert_present "'health'" "post_startup waited for gateway health"
  # The shim reports a CLEAN memory index (files=0, dirty=false, identity
  # valid), so the entrypoint must take the fast path and skip reindex.
  if grep -q "'memory' 'index'" "$LOG"; then
    fail "memory index skipped on clean status (fast path violated)"
  else
    pass "memory index correctly skipped on clean status"
  fi

  # --- phase order: setup < config set < mcp add < cron list (line no.) ---
  local o_setup o_cfg o_mcp o_cron
  o_setup=$(first_line "'setup'"); o_cfg=$(first_line "'config' 'set'")
  o_mcp=$(first_line "'mcp' 'add'"); o_cron=$(first_line "'cron' 'list'")
  if [ -n "$o_setup" ] && [ -n "$o_cfg" ] && [ -n "$o_mcp" ] && [ -n "$o_cron" ] \
    && [ "$o_setup" -lt "$o_cfg" ] && [ "$o_cfg" -lt "$o_mcp" ] \
    && [ "$o_mcp" -lt "$o_cron" ]; then
    pass "phase order setup(l$o_setup) < config set(l$o_cfg) < mcp add(l$o_mcp) < cron list(l$o_cron)"
  else
    fail "phase order setup($o_setup) < config set($o_cfg) < mcp add($o_mcp) < cron list($o_cron)"
  fi
}

echo "[smoke] building $IMAGE (podman/docker build -f container/Dockerfile .)"
"$ENGINE" build --build-arg AGENT_BASE_VERSION=smoke -f container/Dockerfile -t "$IMAGE" .

# --- image contract: gh CLI present for the gh-auth phase ---
# --entrypoint bypasses tini + the boot entrypoint; gh --version exits 0
# only when the binary is installed and runnable.
if "$ENGINE" run --rm --entrypoint gh "$IMAGE" --version >.smoke-gh.log 2>&1; then
  pass "gh CLI present in image"
else
  fail "gh CLI missing from image:" && sed 's/^/    /' .smoke-gh.log >&2
fi

smoke_fixture freya-like ac-infinity
smoke_fixture mimir-like trade-agent

if [ "$FAILURES" -eq 0 ]; then
  rm -f .smoke-*.log
  echo "[smoke] PASS (both fixtures green)"
else
  echo "[smoke] FAIL: $FAILURES assertion(s); logs kept as .smoke-*.log" >&2
  exit 1
fi
