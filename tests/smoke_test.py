#!/usr/bin/env python3
"""Image smoke test: build agent-base once, boot each fixture (freya-like,
mimir-like) against the fake openclaw CLI (tests/shim/openclaw), and assert
the boot's phase order from the shim's invocation log.

Three scenarios:
  1. per fixture: entrypoint --validate-spec — spec + automations parse,
     no mutation
  2. per fixture: inline phase runner — full boot (first boot + reconcile
     + seed + post_startup) WITHOUT the fork/supervise gateway handoff
     (the entrypoint is bypassed, so no gateway process is supervised)
  3. graceful-shutdown drain — the REAL entrypoint chain (tini included)
     with a fake gateway CMD: docker/podman stop must exit 0 only after
     the gateway's in-flight "automation" child finished

The shim log contains resolved arg values by design (that is what the
assertions match); it is a throwaway local artifact, removed on success.
"""

import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
LOGDIR = REPO_ROOT / "logs"
IMAGE = os.environ.get("AGENT_BASE_IMAGE", sys.argv[1] if len(sys.argv) > 1 else "agent-base:smoke")
# SMOKE_ENGINE pins the engine (CI sets docker: GH runners preinstall
# podman, the auto-detect would pick it and build into podman's store —
# same reason CONTRACT_ENGINE exists in tests/contract_test.py).
ENGINE = os.environ.get("SMOKE_ENGINE") or (shutil.which("podman") and "podman") or "docker"

FAILURES = 0

# Mirrors entrypoint.main() minus the fork/supervise handoff. Each run
# starts a fresh container (no volume), so openclaw.json is absent and the
# first-boot path always executes. The shim log is printed after a marker
# line instead of bind-mounting the log file out (rootless uid mapping
# makes single-file mounts unreliable).
RUNNER = """
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

data = entrypoint.data_dir()
def marker(name):
    p = data / name
    v = p.read_text(encoding="utf-8").strip() if p.exists() else "MISSING"
    print(f"=== MARKER: {name}={v} ===")
marker("last-image-version")
marker("agent-managed-mcp")
print("=== SHIM LOG ===")
with open(os.environ["OPENCLAW_SHIM_LOG"], encoding="utf-8") as f:
    sys.stdout.write(f.read())
"""

GATEWAY = """
import signal, subprocess, sys
signal.signal(signal.SIGTERM, lambda *_: None)
child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(3)"])
child.wait()
print("=== DRAIN COMPLETE ===", flush=True)
"""


def pass_(msg: str) -> None:
    print(f"  PASS {msg}")


def fail(msg: str) -> None:
    global FAILURES
    print(f"  FAIL {msg}", file=sys.stderr)
    FAILURES += 1


def run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(argv, capture_output=True, text=True, **kwargs)


def build_common(fixture: str) -> list[str]:
    """Shared run args for every scenario: engine, shim on PATH, dummy env
    for every spec template, fixture content mounted read-only. Call sites
    append the run mode (--rm, or -d --name), the image, and the command."""
    f = REPO_ROOT / "tests" / "fixtures" / fixture
    args = [
        ENGINE,
        "run",
        # Rootless podman on this host denies the container access to
        # bind-mounted repo files under the default MCS relabel (verified
        # for the image user, --user 0:0, and --userns=keep-id); disabling
        # the label relabel fixes it. Safe here: throwaway local test
        # container.
        "--security-opt",
        "label=disable",
        "-e",
        "TELEGRAM_CHAT_ID=123456",
        "-e",
        "TELEGRAM_ALLOWED_USERS=111111,222222",
        "-e",
        "TELEGRAM_TOPIC_MORNING=777",
        "-e",
        "AC_INFINITY_EMAIL=smoke@example.com",
        "-e",
        "AC_INFINITY_PASSWORD=smoke-password",
        "-e",
        "ALPHAVANTAGE_API_KEY=smoke-av-key",
        "-e",
        "LUNARCRUSH_API_KEY=smoke-lc-key",
        "-e",
        "DATABASE_URL=postgres://localhost/smoke",
        "-e",
        "ZAI_API_KEY=smoke-zai-key",
        "-e",
        "AGENT_GIT_TOKEN=smoke-gh-token",
        "-e",
        "AGENT_AUTOMATION_TRIGGERS=1",
        "-e",
        "HOME=/home/node",
        "-e",
        "OPENCLAW_SHIM_LOG=/tmp/shim.log",
        "-e",
        "PATH=/shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "-v",
        f"{REPO_ROOT}/tests/shim:/shim:ro",
        "-v",
        f"{f}/spec.json:/opt/agent/spec.json:ro",
        "-v",
        f"{f}/automations:/opt/agent/automations:ro",
        "-v",
        f"{f}/workspace:/opt/seed/workspace:ro",
        "-v",
        f"{f}/docs:/opt/seed/docs:ro",
    ]
    # Not every fixture ships skills (mimir-like does not); the image's
    # empty /opt/seed/skills placeholder covers it.
    if (f / "skills").is_dir():
        args += ["-v", f"{f}/skills:/opt/seed/skills:ro"]
    # Only trigger-script fixtures ship a scripts dir; the image's empty
    # /opt/agent/scripts placeholder covers the rest.
    if (f / "scripts").is_dir():
        args += ["-v", f"{f}/scripts:/opt/agent/scripts:ro"]
    return args


def smoke_fixture(fixture: str, mcp_name: str, triggers: str = "") -> None:
    print(f"[smoke] fixture: {fixture}")
    validate_log = LOGDIR / f"smoke-{fixture}.validate.log"
    boot_log = LOGDIR / f"smoke-{fixture}.boot.log"
    shim_log = LOGDIR / f"smoke-{fixture}.shim.log"
    common = build_common(fixture)

    # 1) --validate-spec: args after the image replace CMD, so this runs
    #    tini -- python3 /opt/agent/entrypoint.py --validate-spec.
    proc = run(common + ["--rm", IMAGE, "--validate-spec"])
    validate_log.write_text(proc.stdout + proc.stderr, encoding="utf-8")
    if proc.returncode == 0:
        pass_("--validate-spec accepted spec + automations")
    else:
        fail(f"--validate-spec rejected the fixture:\n{indent(proc.stdout + proc.stderr)}")
        return

    # 2) full boot via the inline runner (tini bypassed; command = python3).
    proc = run(common + ["--rm", "--entrypoint", "python3", IMAGE, "-c", RUNNER])
    boot = proc.stdout + proc.stderr
    boot_log.write_text(boot, encoding="utf-8")
    if proc.returncode != 0:
        fail(f"full boot exited nonzero:\n{indent(boot)}")
        return
    pass_("full boot (first boot + reconcile + seed + post_startup) exited 0")
    marker = boot.find("=== SHIM LOG ===\n")
    log = boot[boot.find("\n", marker) + 1 :] if marker >= 0 else ""
    shim_log.write_text(log, encoding="utf-8")

    # --- X1 phase markers (printed by the runner from {data}) ---
    if "=== MARKER: last-image-version=smoke ===" in boot:
        pass_("upgrade-backup phase recorded image version (fresh volume, no backup)")
    else:
        fail("upgrade-backup phase did not record the image version")
    if "=== MARKER: agent-managed-mcp=[" in boot:
        pass_("managed-mcp marker written (ownership tracking active)")
    else:
        fail("managed-mcp marker missing")

    # --- shim.log assertions (args are single-quoted per arg in the log) ---
    def assert_present(pattern: str, description: str) -> None:
        if re.search(pattern, log):
            pass_(description)
        else:
            fail(f"{description} (pattern not found: {pattern})")

    assert_present(r"'setup'", "first boot: openclaw setup ran")
    assert_present(r"'config' 'set'", "reconcile_config applied config entries")
    assert_present(rf"'mcp' 'add' '{mcp_name}'", f"reconcile_mcp registered '{mcp_name}'")
    assert_present(r"'cron' 'list'", "post_startup seeded cron jobs (cron list)")
    assert_present(r"'--tools'", "seeded cron jobs carry a bounded tool allow-list")
    assert_present(r"'--failure-alert'", "seeded cron jobs alert on failed/skipped runs")
    assert_present(r"'memory' 'status'", "memory ladder checked index status")
    assert_present(r"'health'", "post_startup waited for gateway health")
    # --- trigger-script surface (opt-in env + read-only scripts mount) ---
    if triggers:
        assert_present(
            r"'config' 'set' 'cron.triggers.enabled' 'true'",
            "trigger automations armed cron.triggers.enabled before seeding",
        )
        assert_present(
            r"'--trigger-script' '/opt/agent/scripts/probe.js'",
            "trigger job seeded via --trigger-script",
        )
    elif "'cron.triggers.enabled'" in log:
        fail("no trigger arming without trigger-script automations")
    else:
        pass_("no trigger arming without trigger-script automations")
    # The shim reports a CLEAN memory index (files=0, dirty=false, identity
    # valid), so the entrypoint must take the fast path and skip reindex.
    if "'memory' 'index'" in log:
        fail("memory index skipped on clean status (fast path violated)")
    else:
        pass_("memory index correctly skipped on clean status")

    # --- phase order: setup < config set < mcp add < cron list (line no.) ---
    def first_line(pattern: str):
        m = re.search(pattern, log)
        return log.count("\n", 0, m.start()) + 1 if m else None

    o_setup, o_cfg = first_line(r"'setup'"), first_line(r"'config' 'set'")
    o_mcp, o_cron = first_line(r"'mcp' 'add'"), first_line(r"'cron' 'list'")
    order = (o_setup, o_cfg, o_mcp, o_cron)
    if None not in order and o_setup < o_cfg < o_mcp < o_cron:
        pass_(
            f"phase order setup(l{o_setup}) < config set(l{o_cfg}) <"
            f" mcp add(l{o_mcp}) < cron list(l{o_cron})"
        )
    else:
        fail(
            f"phase order setup({o_setup}) < config set({o_cfg}) <"
            f" mcp add({o_mcp}) < cron list({o_cron})"
        )


def smoke_drain() -> None:
    """Graceful shutdown through the REAL entrypoint chain (tini included):
    the CMD is a fake gateway that traps SIGTERM and runs a 3s in-flight
    "automation" child. A stop must exit 0 only after that child finished
    — the marker prints after child.wait(), so its presence in the logs
    proves the drain; marker asserts only, no timing asserts."""
    print("[smoke] graceful shutdown drain")
    name = f"agent-base-smoke-drain-{os.getpid()}"
    run_log = LOGDIR / "smoke-drain.run.log"
    drain_log = LOGDIR / "smoke-drain.log"
    common = build_common("freya-like")

    run([ENGINE, "rm", "-f", name])
    proc = run(common + ["-d", "--name", name, IMAGE, "python3", "-u", "-c", GATEWAY])
    run_log.write_text(proc.stdout + proc.stderr, encoding="utf-8")
    if proc.returncode != 0:
        fail(f"drain: container failed to start:\n{indent(proc.stdout + proc.stderr)}")
        run([ENGINE, "rm", "-f", name])
        return
    # Boot phases + gateway spawn; the shim answers health instantly, the
    # margin covers cold starts.
    time.sleep(8)
    run([ENGINE, "stop", "-t", "30", name])
    logs = run([ENGINE, "logs", name])
    drain_log.write_text(logs.stdout + logs.stderr, encoding="utf-8")
    exit_code = run([ENGINE, "inspect", "-f", "{{.State.ExitCode}}", name]).stdout.strip()
    run([ENGINE, "rm", "-f", name])

    if exit_code == "0":
        pass_("drain: container stopped with exit 0")
    else:
        fail(
            f"drain: container exit code was '{exit_code}' (expected 0):\n"
            f"{indent(logs.stdout + logs.stderr)}"
        )
    if "=== DRAIN COMPLETE ===" in logs.stdout + logs.stderr:
        pass_("drain: in-flight automation child finished before exit")
    else:
        fail("drain: in-flight automation child did not complete")


def indent(text: str) -> str:
    return "".join(f"    {line}\n" for line in text.splitlines())


def main() -> int:
    LOGDIR.mkdir(parents=True, exist_ok=True)
    print(f"[smoke] building {IMAGE} ({ENGINE} build -f container/Dockerfile .)")
    # OCI format drops the HEALTHCHECK under podman; keep it in the smoke
    # artifact so the inspect assertion below is meaningful.
    format_flag = ["--format", "docker"] if ENGINE == "podman" else []
    proc = run(
        [
            ENGINE,
            "build",
            *format_flag,
            "--build-arg",
            "AGENT_BASE_VERSION=smoke",
            "-f",
            "container/Dockerfile",
            "-t",
            IMAGE,
            ".",
        ],
        cwd=REPO_ROOT,
    )
    if proc.returncode != 0:
        print(proc.stdout + proc.stderr, file=sys.stderr)
        return 1

    # --- image contract: gh CLI present for the gh-auth phase ---
    # --entrypoint bypasses tini + the boot entrypoint; gh --version exits
    # 0 only when the binary is installed and runnable.
    gh = run([ENGINE, "run", "--rm", "--entrypoint", "gh", IMAGE, "--version"])
    (LOGDIR / "smoke-gh.log").write_text(gh.stdout + gh.stderr, encoding="utf-8")
    if gh.returncode == 0:
        pass_("gh CLI present in image")
    else:
        fail("gh CLI missing from image")

    healthcheck = run(
        [ENGINE, "inspect", "--type", "image", IMAGE, "--format", "{{.Config.Healthcheck.Test}}"]
    )
    if "healthz" in healthcheck.stdout:
        pass_("image carries HEALTHCHECK (/healthz)")
    else:
        fail("image HEALTHCHECK missing (was the build OCI-format?)")

    smoke_fixture("freya-like", "ac-infinity", triggers="yes")
    smoke_fixture("mimir-like", "trade-agent")
    smoke_drain()

    if FAILURES == 0:
        for stale in LOGDIR.glob("smoke-*.log"):
            stale.unlink()
        print("[smoke] PASS (both fixtures green)")
        return 0
    print(
        f"[smoke] FAIL: {FAILURES} assertion(s); logs kept in {LOGDIR}/smoke-*.log", file=sys.stderr
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
