#!/usr/bin/env python3
"""CLI contract test: proves agent-base's emitted openclaw argv against the
REAL OpenClaw CLI inside the built image — the shim is not consulted for any
verdict (roadmap N4; the shim-only smoke let the --type remote bug ship).

Four stages:
  A) boot the image with the shim on PATH purely to CAPTURE the argv the
     entrypoint emits for the canary spec — twice, against a persisted
     shim job store: boot 1 seeds from empty; boot 2 sees the shim's
     payload-less stored jobs and emits the drift-heal `cron edit` argv,
     so edit-only flags get policed too
  B) cross-check every captured flag against `openclaw <cmd> --help` run
     with the real CLI (flag drift fails here)
  C) boot the image with NO shim and the real CLI: full first boot +
     reconcile must exit 0 with zero [agent-entry] [warn] lines, and
      `mcp list --json` must contain both canary servers
  D) upgrade path: boot the LAST PUBLISHED release on a fresh volume with
     the stable minimal spec dialect (spec.upgrade.json + the old-dialect
     upgrade automations dir), then the candidate on the SAME volume —
     the version delta must take a verified backup to /backups,
     reconcile with zero [warn]s, advance the last-image-version marker,
     and keep both upgrade-spec MCP servers registered

Usage: python3 scripts/contract_test.py [IMAGE]   (builds nothing; expects
the tag given, or ghcr.io/tankdonut/agent-base:contract)

Environment:
  CONTRACT_ENGINE          podman/docker override (default: auto-detect,
                           podman first)
  CONTRACT_UPGRADE         auto | required | off — stage D mode. auto (the
                           default) lets discovery/pull preconditions
                           warn-and-skip for offline local runs; required
                           (CI) fails them. Assertion failures always fail.
  CONTRACT_BASELINE_IMAGE  fully-qualified stage D baseline override (debug)

spec.upgrade.json is a stable minimal dialect old loaders accept — new
schema keys must NOT be added there (they prove themselves via the canary
spec in stages A-C).
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request
from pathlib import Path
from typing import NoReturn

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_IMAGE = "ghcr.io/tankdonut/agent-base:contract"
GHCR_REPO = "tankdonut/agent-base"
SHIM_PATH = "/shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
DATE_TAG_RE = re.compile(r"\d{4}\.\d{2}\.\d{2}(?:\.\d+)?\Z")

# Subcommand paths agent-base invokes (longest-prefix match against the
# shim log). `gateway` is the supervised CMD, not a CLI call — excluded.
CLI_PATHS = (
    "setup",
    "models fallbacks add",
    "channels add",
    "plugins install",
    "plugins list",
    "config set",
    "config validate",
    "mcp list",
    "mcp add",
    "health",
    "memory status",
    "memory index",
    "doctor",
    "security audit",
    "cron list",
    "cron add",
    "cron edit",
    "cron delete",
)

CANARY_ENV = (
    "-e",
    "ZAI_API_KEY=contract-dummy-zai",
    "-e",
    "CONTRACT_REMOTE_TOKEN=contract-dummy-bearer",
    "-e",
    "AGENT_AUTOMATION_TRIGGERS=1",
    "-e",
    "HOME=/home/node",
)
CANARY_MOUNTS = (
    "--security-opt",
    "label=disable",
    "-v",
    f"{REPO_ROOT}/scripts/contract/spec.json:/opt/agent/spec.json:ro",
    "-v",
    f"{REPO_ROOT}/scripts/contract/automations:/opt/agent/automations:ro",
    "-v",
    f"{REPO_ROOT}/scripts/contract/scripts:/opt/agent/scripts:ro",
)
UPGRADE_MOUNTS = (
    "--security-opt",
    "label=disable",
    "-v",
    f"{REPO_ROOT}/scripts/contract/spec.upgrade.json:/opt/agent/spec.json:ro",
    "-v",
    f"{REPO_ROOT}/scripts/contract/upgrade-automations:/opt/agent/automations:ro",
)


def pass_line(msg: str) -> None:
    print(f"  PASS {msg}")


def warn_line(msg: str) -> None:
    print(f"  WARN {msg}")


def fail(msg: str, detail: str = "") -> NoReturn:
    print(f"  FAIL {msg}", file=sys.stderr)
    if detail:
        print("\n".join(f"    {line}" for line in detail.splitlines()), file=sys.stderr)
    sys.exit(1)


def detect_engine() -> str:
    engine = os.environ.get("CONTRACT_ENGINE", "").strip()
    if engine:
        return engine
    for candidate in ("podman", "docker"):
        if shutil.which(candidate):
            return candidate
    fail("no container engine found — podman or docker is required")


def engine_run(
    engine: str, args: list[str], timeout: float
) -> subprocess.CompletedProcess[str] | None:
    """One engine invocation; None maps a timeout onto the failure path."""
    try:
        return subprocess.run(
            [engine, *args], capture_output=True, text=True, timeout=timeout, check=False
        )
    except subprocess.TimeoutExpired:
        return None


def output_of(proc: subprocess.CompletedProcess[str] | None) -> str:
    if proc is None:
        return "<timed out>"
    return (proc.stdout or "") + (proc.stderr or "")


def succeeded(proc: subprocess.CompletedProcess[str] | None) -> bool:
    return proc is not None and proc.returncode == 0


def stage_validate(engine: str, image: str) -> None:
    proc = engine_run(
        engine,
        ["run", "--rm", *CANARY_MOUNTS, *CANARY_ENV, image, "--validate-spec"],
        timeout=120,
    )
    if succeeded(proc):
        pass_line("--validate-spec accepted canary spec")
    else:
        fail("canary spec rejected", output_of(proc))


def stage_a(engine: str, image: str, work: Path) -> Path:
    # Two boots sharing the shim's job store via a persisted /persist
    # mount (the shim keeps its jobs file beside its log). Boot 1 seeds
    # from empty; boot 2 re-lists those payload-less jobs and emits the
    # drift-heal `cron edit` argv. If boot 1's seeding misses the sleep-3
    # window, boot 2 degrades to adds — less coverage that run, never a
    # false fail.
    persist = work / "persist"
    persist.mkdir()
    # The container writes as mapped uid 1000, not the host user — the
    # log and job store must stay writable across both boots.
    os.chmod(persist, 0o777)
    shim_log = persist / "shim.log"
    for boot in (1, 2):
        proc = engine_run(
            engine,
            [
                "run",
                "--rm",
                "-v",
                f"{REPO_ROOT}/scripts/shim:/shim:ro",
                "-v",
                f"{persist}:/persist",
                "-e",
                "OPENCLAW_SHIM_LOG=/persist/shim.log",
                "-e",
                f"PATH={SHIM_PATH}",
                *CANARY_MOUNTS,
                *CANARY_ENV,
                image,
                "sleep",
                "3",
            ],
            timeout=120,
        )
        log = work / f"stage-a{boot}.log"
        log.write_text(output_of(proc), encoding="utf-8")
        if succeeded(proc):
            pass_line(f"stage A boot {boot}: shim boot exited 0")
        else:
            fail(f"stage A boot {boot} failed", log.read_text(encoding="utf-8"))
    if not shim_log.exists():
        fail("stage A: shim log missing after boots")
    return shim_log


def match_cli_path(tokens: list[str]) -> str | None:
    for path in CLI_PATHS:
        words = path.split()
        if tokens[: len(words)] == words:
            return path
    return None


def stage_b(engine: str, image: str, shim_log: Path) -> None:
    help_text = {
        path: output_of(
            engine_run(
                engine,
                [
                    "run",
                    "--rm",
                    "--entrypoint",
                    "openclaw",
                    *CANARY_ENV,
                    image,
                    *path.split(),
                    "--help",
                ],
                timeout=60,
            )
        )
        for path in CLI_PATHS
    }

    violations: list[str] = []
    for raw in shim_log.read_text(encoding="utf-8").splitlines():
        line = re.sub(r"^cwd=\S+\s+", "", raw, count=1)
        tokens = re.findall(r"'([^']*)'", line)
        matched = match_cli_path(tokens) if tokens else None
        if matched is None:
            continue
        for token in tokens:
            if not token.startswith("--"):
                continue
            if not re.search(rf"(^|[\s,]){re.escape(token)}\b", help_text[matched]):
                violations.append(
                    f"flag '{token}' (from: {matched}) missing from '{matched} --help'"
                )
    if violations:
        fail(f"stage B: {len(violations)} flag(s) drifted from the real CLI", "\n".join(violations))
    pass_line("stage B: every emitted flag exists in real CLI help")


def stage_c(engine: str, image: str, volume: str, work: Path) -> None:
    proc = engine_run(
        engine,
        [
            "run",
            "--rm",
            "-v",
            f"{volume}:/home/node/.openclaw",
            *CANARY_MOUNTS,
            *CANARY_ENV,
            image,
            "sleep",
            "5",
        ],
        timeout=420,
    )
    log = work / "stage-c.log"
    log.write_text(output_of(proc), encoding="utf-8")
    if not succeeded(proc):
        fail("stage C real boot failed", log.read_text(encoding="utf-8"))
    pass_line("stage C: real boot exited 0")
    warn_lines = [
        line
        for line in log.read_text(encoding="utf-8").splitlines()
        if "[agent-entry] [warn]" in line
    ]
    if warn_lines:
        fail("stage C: boot log carries [warn] lines", "\n".join(warn_lines))
    pass_line("stage C: zero [warn] lines in boot log")

    mcp = engine_run(
        engine,
        [
            "run",
            "--rm",
            "--entrypoint",
            "openclaw",
            "-v",
            f"{volume}:/home/node/.openclaw",
            *CANARY_ENV,
            image,
            "mcp",
            "list",
            "--json",
        ],
        timeout=60,
    )
    mcp_out = output_of(mcp)
    if not succeeded(mcp):
        fail("mcp list --json failed against booted volume", mcp_out)
    for needle in ('"contract-remote"', "mcp.invalid", '"contract-local"'):
        if needle in mcp_out:
            pass_line(f"stage C: mcp list contains {needle}")
        else:
            fail(f"stage C: mcp list missing {needle}", mcp_out)


def discover_baseline() -> str:
    """Newest published YYYY.MM.DD[.N] tag on GHCR (anonymous; public package)."""
    scope = urllib.parse.quote(f"repository:{GHCR_REPO}:pull")
    with urllib.request.urlopen(
        f"https://ghcr.io/token?service=ghcr.io&scope={scope}", timeout=30
    ) as resp:
        token = json.load(resp)["token"]
    tags: list[str] = []
    last: str | None = None
    while True:
        url = f"https://ghcr.io/v2/{GHCR_REPO}/tags/list?n=1000"
        if last is not None:
            url += f"&last={urllib.parse.quote(last)}"
        request = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
        with urllib.request.urlopen(request, timeout=30) as resp:
            page = json.load(resp).get("tags", [])
        if not page:
            break
        tags.extend(page)
        last = page[-1]
        if len(page) < 1000:
            break
    dated = [tag for tag in tags if DATE_TAG_RE.fullmatch(tag)]
    if not dated:
        return ""
    newest = max(dated, key=lambda tag: tuple(int(part) for part in tag.split(".")))
    return f"ghcr.io/{GHCR_REPO}:{newest}"


def volume_cat(engine: str, volume: str, baseline: str, path: str) -> str:
    proc = engine_run(
        engine,
        [
            "run",
            "--rm",
            "--entrypoint",
            "/bin/sh",
            "-v",
            f"{volume}:/home/node/.openclaw",
            baseline,
            "-c",
            f"cat {path} 2>/dev/null || true",
        ],
        timeout=60,
    )
    return (proc.stdout if proc else "").strip()


def stage_d(
    engine: str, image: str, upgrade_volume: str, backup_volume: str, work: Path, mode: str
) -> bool:
    def precondition(msg: str, detail: str = "") -> None:
        if mode == "required":
            fail(f"stage D: {msg}", detail)
        warn_line(f"stage D: {msg} — skipping upgrade-path scenario")
        if detail:
            print(
                "\n".join(f"    {line}" for line in detail.splitlines()),
                file=sys.stderr,
            )

    baseline = os.environ.get("CONTRACT_BASELINE_IMAGE", "").strip()
    if not baseline:
        try:
            baseline = discover_baseline()
        except OSError:
            baseline = ""
    if not baseline:
        precondition("no published baseline tag discovered (GHCR unreachable?)")
        return False

    inspect = engine_run(
        engine,
        [
            "image",
            "inspect",
            "--format",
            "{{range .Config.Env}}{{println .}}{{end}}",
            image,
        ],
        timeout=60,
    )
    cand_version = ""
    for env_line in output_of(inspect).splitlines():
        if env_line.startswith("AGENT_BASE_VERSION="):
            cand_version = env_line.split("=", 1)[1].strip()
            break
    if not cand_version:
        precondition("candidate image carries no AGENT_BASE_VERSION (dev build?)")
        return False

    print(f"[contract] stage D baseline: {baseline} (candidate version: {cand_version})")
    pull = engine_run(engine, ["pull", "-q", baseline], timeout=300)
    if not succeeded(pull):
        precondition(f"baseline pull failed: {baseline}", output_of(pull))
        return False

    boot1 = engine_run(
        engine,
        [
            "run",
            "--rm",
            "-v",
            f"{upgrade_volume}:/home/node/.openclaw",
            "-v",
            f"{backup_volume}:/backups",
            *UPGRADE_MOUNTS,
            *CANARY_ENV,
            baseline,
            "sleep",
            "5",
        ],
        timeout=420,
    )
    log1 = work / "stage-d1.log"
    log1.write_text(output_of(boot1), encoding="utf-8")
    if not succeeded(boot1):
        fail("stage D baseline boot failed", log1.read_text(encoding="utf-8"))
    pass_line("stage D: baseline boot exited 0")

    marker1 = volume_cat(
        engine, upgrade_volume, baseline, "/home/node/.openclaw/last-image-version"
    )
    if not marker1:
        fail("stage D: baseline boot recorded no last-image-version marker")
    if marker1 == cand_version:
        print(
            f"  SKIP stage D (candidate AGENT_BASE_VERSION equals baseline marker "
            f"'{marker1}' — no upgrade delta)"
        )
        return False

    boot2 = engine_run(
        engine,
        [
            "run",
            "--rm",
            "-v",
            f"{upgrade_volume}:/home/node/.openclaw",
            "-v",
            f"{backup_volume}:/backups",
            *UPGRADE_MOUNTS,
            *CANARY_ENV,
            image,
            "sleep",
            "5",
        ],
        timeout=420,
    )
    log2 = work / "stage-d2.log"
    log2.write_text(output_of(boot2), encoding="utf-8")
    if not succeeded(boot2):
        fail("stage D upgrade boot failed", log2.read_text(encoding="utf-8"))
    pass_line("stage D: upgrade boot exited 0")

    log2_text = log2.read_text(encoding="utf-8")
    if "Image changed (" not in log2_text:
        fail(
            "stage D: no verified backup on the delta (missing 'Image changed (' line)",
            "\n".join(line for line in log2_text.splitlines() if "agent-entry" in line),
        )
    pass_line("stage D: verified backup taken on version delta")
    warn_lines = [line for line in log2_text.splitlines() if "[agent-entry] [warn]" in line]
    if warn_lines:
        fail("stage D: upgrade boot log carries [warn] lines", "\n".join(warn_lines))
    pass_line("stage D: zero [warn] lines in upgrade boot log")

    backup_ls = engine_run(
        engine,
        [
            "run",
            "--rm",
            "--entrypoint",
            "/bin/sh",
            "-v",
            f"{backup_volume}:/backups",
            baseline,
            "-c",
            "ls -A /backups 2>/dev/null | wc -l",
        ],
        timeout=60,
    )
    backup_count = (backup_ls.stdout if backup_ls else "").strip()
    if backup_count.isdigit() and int(backup_count) > 0:
        pass_line("stage D: backup archive present in /backups")
    else:
        fail("stage D: /backups is empty after upgrade boot")

    marker2 = volume_cat(
        engine, upgrade_volume, baseline, "/home/node/.openclaw/last-image-version"
    )
    if marker2 == cand_version:
        pass_line(f"stage D: last-image-version marker advanced to '{marker2}'")
    else:
        missing = marker2 or "<empty>"
        fail(f"stage D: marker is '{missing}', expected '{cand_version}'")

    mcp = engine_run(
        engine,
        [
            "run",
            "--rm",
            "--entrypoint",
            "openclaw",
            "-v",
            f"{upgrade_volume}:/home/node/.openclaw",
            *CANARY_ENV,
            image,
            "mcp",
            "list",
            "--json",
        ],
        timeout=60,
    )
    mcp_out = output_of(mcp)
    if not succeeded(mcp):
        fail("stage D: mcp list --json failed against upgraded volume", mcp_out)
    for needle in ('"contract-upgrade-remote"', '"contract-upgrade-local"'):
        if needle in mcp_out:
            pass_line(f"stage D: mcp list contains {needle}")
        else:
            fail(f"stage D: mcp list missing {needle}", mcp_out)
    return True


def main() -> int:
    engine = detect_engine()
    image = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_IMAGE
    mode = os.environ.get("CONTRACT_UPGRADE", "auto").strip() or "auto"
    print(f"[contract] image: {image} (engine: {engine})")

    pid = os.getpid()
    volume = f"contract-data-{pid}"
    upgrade_volume = f"contract-upgrade-{pid}"
    backup_volume = f"contract-backups-{pid}"
    work = Path(tempfile.mkdtemp(prefix="contract-"))
    try:
        stage_validate(engine, image)
        shim_log = stage_a(engine, image, work)
        stage_b(engine, image, shim_log)
        stage_c(engine, image, volume, work)
        if mode == "off":
            print("  SKIP stage D (CONTRACT_UPGRADE=off)")
            upgrade_ran = False
        else:
            upgrade_ran = stage_d(engine, image, upgrade_volume, backup_volume, work, mode)
        tail = "upgrade path verified" if upgrade_ran else "upgrade path skipped"
        print(f"[contract] PASS (argv matches real CLI; real boot clean; {tail})")
        return 0
    finally:
        for vol in (volume, upgrade_volume, backup_volume):
            subprocess.run(
                [engine, "volume", "rm", "-f", vol],
                capture_output=True,
                timeout=60,
                check=False,
            )
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
