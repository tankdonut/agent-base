#!/usr/bin/env bash
# Image smoke test: build agent-base once, boot each fixture (freya-like,
# mimir-like) against the fake openclaw CLI (scripts/shim/openclaw), and
# assert the boot's phase order from the shim's invocation log.
#
# Three scenarios:
#   1. per fixture: entrypoint --validate-spec — spec + automations parse,
#      no mutation
#   2. per fixture: inline phase runner — full boot (first boot + reconcile
#      + seed + post_startup) WITHOUT the fork/supervise gateway handoff
#      (the entrypoint is bypassed, so no gateway process is supervised)
#   3. graceful-shutdown drain — the REAL entrypoint chain (tini included)
#      with a fake gateway CMD: docker/podman stop must exit 0 only after
#      the gateway's in-flight "automation" child finished
#
# The shim log contains resolved arg values by design (that is what the
# assertions grep); it is a throwaway local artifact, removed on success.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT"

# Log artifacts land in ./logs (gitignored) — never loose in the repo root.
LOGDIR="$REPO_ROOT/logs"
mkdir -p "$LOGDIR"

IMAGE=${AGENT_BASE_IMAGE:-${1:-agent-base:smoke}}
# SMOKE_ENGINE pins the engine (CI sets docker: GH runners preinstall
# podman, the auto-detect would pick it and build into podman's store —
# same reason CONTRACT_ENGINE exists in scripts/contract-test.sh).
ENGINE=${SMOKE_ENGINE:-$(command -v podman >/dev/null 2>&1 && echo podman || echo docker)}

FAILURES=0
pass() { printf '  PASS %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; FAILURES=$((FAILURES + 1)); }

# Mirrors entrypoint.main() minus the fork/supervise handoff. Each run starts
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
entrypoint.backup_before_upgrade(env)

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

print(f"=== MARKER: version={(entrypoint.data_dir() / 'last-image-version').read_text(encoding='utf-8').strip() if (entrypoint.data_dir() / 'last-image-version').exists() else 'MISSING'} ===")
print(f"=== MARKER: managed-mcp={(entrypoint.data_dir() / 'agent-managed-mcp').read_text(encoding='utf-8').strip() if (entrypoint.data_dir() / 'agent-managed-mcp').exists() else 'MISSING'} ===")
print("=== SHIM LOG ===")
with open(os.environ["OPENCLAW_SHIM_LOG"], encoding="utf-8") as f:
    sys.stdout.write(f.read())
EOF

assert_present() { # assert_present PATTERN DESCRIPTION
  if grep -q -- "$1" "$LOG"; then pass "$2"; else fail "$2 (pattern not found: $1)"; fi
}
first_line() { grep -n -- "$1" "$LOG" | head -n 1 | cut -d: -f1; }

# Shared run args for every scenario (engine, shim on PATH, dummy env for
# every spec template, fixture content mounted read-only). Call sites
# append the run mode (--rm, or -d --name), the image, and the command.
build_common() { # build_common FIXTURE
  local f=$1
  COMMON_ARGS=(
    "$ENGINE" run
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
    -e "ZAI_API_KEY=smoke-zai-key"
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
    COMMON_ARGS+=(-v "$REPO_ROOT/fixtures/$f/skills:/opt/seed/skills:ro")
  fi
}

smoke_fixture() { # smoke_fixture FIXTURE EXPECTED_MCP_NAME
  local f=$1 mcp=$2
  local validate_log="$LOGDIR/smoke-$f.validate.log" boot_log="$LOGDIR/smoke-$f.boot.log"
  LOG="$LOGDIR/smoke-$f.shim.log"
  echo "[smoke] fixture: $f"
  build_common "$f"

  # 1) --validate-spec: args after the image replace CMD, so this runs
  #    tini -- python3 /opt/agent/entrypoint.py --validate-spec.
  if "${COMMON_ARGS[@]}" --rm "$IMAGE" --validate-spec >"$validate_log" 2>&1; then
    pass "--validate-spec accepted spec + automations"
  else
    fail "--validate-spec rejected the fixture:" && sed 's/^/    /' "$validate_log" >&2
    return 0
  fi

  # 2) full boot via the inline runner (tini bypassed; command = python3).
  if "${COMMON_ARGS[@]}" --rm --entrypoint python3 "$IMAGE" -c "$RUNNER" >"$boot_log" 2>&1; then
    pass "full boot (first boot + reconcile + seed + post_startup) exited 0"
  else
    fail "full boot exited nonzero:" && sed 's/^/    /' "$boot_log" >&2
    return 0
  fi
  sed -n '/^=== SHIM LOG ===$/,$p' "$boot_log" | tail -n +2 >"$LOG"

  # --- X1 phase markers (printed by the runner from {data}) ---
  if grep -q "^=== MARKER: version=smoke ===$" "$boot_log"; then
    pass "upgrade-backup phase recorded image version (fresh volume, no backup)"
  else
    fail "upgrade-backup phase did not record the image version"
  fi
  if grep -q "^=== MARKER: managed-mcp=\[" "$boot_log"; then
    pass "managed-mcp marker written (ownership tracking active)"
  else
    fail "managed-mcp marker missing"
  fi

  # --- shim.log assertions (args are single-quoted per arg in the log) ---
  assert_present "'setup'" "first boot: openclaw setup ran"
  assert_present "'config' 'set'" "reconcile_config applied config entries"
  assert_present "'mcp' 'add' '$mcp'" "reconcile_mcp registered '$mcp'"
  assert_present "'cron' 'list'" "post_startup seeded cron jobs (cron list)"
  assert_present "'--tools'" "seeded cron jobs carry a bounded tool allow-list"
  assert_present "'--failure-alert'" "seeded cron jobs alert on failed/skipped runs"
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

# 3) graceful shutdown through the REAL entrypoint chain (tini included):
#    the CMD is a fake gateway that traps SIGTERM and runs a 3s in-flight
#    "automation" child. A stop must exit 0 only after that child finished
#    — the marker prints after child.wait(), so its presence in the logs
#    proves the drain; marker asserts only, no timing asserts.
smoke_drain() {
  local name="agent-base-smoke-drain-$$"
  local log="$LOGDIR/smoke-drain.log" run_log="$LOGDIR/smoke-drain.run.log"
  echo "[smoke] graceful shutdown drain"
  build_common freya-like

  local gateway
  gateway=$(cat <<'EOF'
import signal, subprocess, sys
signal.signal(signal.SIGTERM, lambda *_: None)
child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(3)"])
child.wait()
print("=== DRAIN COMPLETE ===", flush=True)
EOF
)

  "$ENGINE" rm -f "$name" >/dev/null 2>&1 || true
  if ! "${COMMON_ARGS[@]}" -d --name "$name" "$IMAGE" python3 -u -c "$gateway" \
      >"$run_log" 2>&1; then
    fail "drain: container failed to start:"$'\n'"$(sed 's/^/    /' "$run_log")"
    "$ENGINE" rm -f "$name" >/dev/null 2>&1 || true
    return 0
  fi
  # Boot phases + gateway spawn; the shim answers health instantly, the
  # margin covers cold starts.
  sleep 8
  "$ENGINE" stop -t 30 "$name" >>"$run_log" 2>&1
  "$ENGINE" logs "$name" >"$log" 2>&1
  local exit_code
  exit_code=$("$ENGINE" inspect -f "{{.State.ExitCode}}" "$name")
  "$ENGINE" rm -f "$name" >/dev/null 2>&1 || true

  if [ "$exit_code" = "0" ]; then
    pass "drain: container stopped with exit 0"
  else
    fail "drain: container exit code was '$exit_code' (expected 0):"$'\n'"$(sed 's/^/    /' "$log")"
  fi
  if grep -q "=== DRAIN COMPLETE ===" "$log"; then
    pass "drain: in-flight automation child finished before exit"
  else
    fail "drain: in-flight automation child did not complete:"$'\n'"$(sed 's/^/    /' "$log")"
  fi
}

echo "[smoke] building $IMAGE (podman/docker build -f container/Dockerfile .)"
PODMAN_FORMAT_FLAG=""
if [ "$ENGINE" = podman ]; then
  # OCI format drops the HEALTHCHECK; keep it in the smoke artifact so the
  # inspect assertion below is meaningful.
  PODMAN_FORMAT_FLAG="--format docker"
fi
"$ENGINE" build $PODMAN_FORMAT_FLAG --build-arg AGENT_BASE_VERSION=smoke -f container/Dockerfile -t "$IMAGE" .

# --- image contract: gh CLI present for the gh-auth phase ---
# --entrypoint bypasses tini + the boot entrypoint; gh --version exits 0
# only when the binary is installed and runnable.
if "$ENGINE" run --rm --entrypoint gh "$IMAGE" --version >"$LOGDIR/smoke-gh.log" 2>&1; then
  pass "gh CLI present in image"
else
  fail "gh CLI missing from image"
fi

if "$ENGINE" inspect --type image "$IMAGE" --format '{{.Config.Healthcheck.Test}}' 2>/dev/null | grep -q "healthz"; then
  pass "image carries HEALTHCHECK (/healthz)"
else
  fail "image HEALTHCHECK missing (was the build OCI-format?)"
fi

smoke_fixture freya-like ac-infinity
smoke_fixture mimir-like trade-agent
smoke_drain

if [ "$FAILURES" -eq 0 ]; then
  rm -f "$LOGDIR"/smoke-*.log
  echo "[smoke] PASS (both fixtures green)"
else
  echo "[smoke] FAIL: $FAILURES assertion(s); logs kept in $LOGDIR/smoke-*.log" >&2
  exit 1
fi
