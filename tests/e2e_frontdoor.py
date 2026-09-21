"""Slim front-door e2e — the shipped-binary user journey, PR-gated.

The full matrix lives in tests/agentctl_e2e.py (nightly). This script
asserts only the journey a real user walks with the built binary:

    agentctl init → secrets init → fleet deploy --all → healthy →
    fleet serve API answers → destroy removes everything

Mechanics (drift parsing, gateway protocol scopes, plane boot,
upgrades) are owned by `go test -tags=integration` — failures here are
binary/UX regressions, not mechanics regressions.
"""

from __future__ import annotations

import json
import os
import random
import shutil
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "tests"))

import agentctl_e2e as full  # noqa: E402 — helper reuse from the full suite

ENGINE = os.environ.get("E2E_ENGINE") or (shutil.which("podman") and "podman") or "docker"
BASE_IMAGE = os.environ.get("AGENT_E2E_IMAGE", full.BASE_IMAGE)

FAILURES = 0
T0 = time.monotonic()


def stage(label: str) -> None:
    print(f"[e2e] {label} (+{time.monotonic() - T0:.0f}s)")


def pass_(msg: str) -> None:
    print(f"  PASS {msg}")


def fail(msg: str) -> None:
    global FAILURES
    print(f"  FAIL {msg}", file=sys.stderr)
    FAILURES += 1


def run(argv: list[str], **kw) -> subprocess.CompletedProcess:
    proc = subprocess.run(argv, capture_output=True, text=True, **kw)
    print(f"  $ {' '.join(str(a) for a in argv)}  (exit={proc.returncode})")
    return proc


def healthy(url: str, budget: float) -> bool:
    deadline = time.monotonic() + budget
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=3) as resp:
                if resp.status == 200:
                    return True
        except Exception:
            pass
        time.sleep(3)
    return False


def main() -> int:
    global FAILURES
    if shutil.which("go") is None:
        print("go is required to build the agentctl binary", file=sys.stderr)
        return 2
    stage("building agentctl")
    bin_dir = Path(tempfile_bin()) / "e2e-frontdoor-bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    agentctl = bin_dir / "agentctl"
    build = run(["go", "build", "-o", str(agentctl), "./cmd/agentctl"], cwd=REPO)
    if build.returncode != 0:
        print(build.stderr, file=sys.stderr)
        return 2

    port = random.randint(20000, 24999)
    project = Path(tempfile_bin()) / f"frontdoor-{random.randint(100000, 999999)}"
    project.mkdir(parents=True, exist_ok=True)
    key = "frontdoor"

    try:
        stage("init + secrets")
        proc = run(
            [
                str(agentctl),
                "init",
                str(project),
                "--base-tag",
                "2099.12.31",
                "--gateway-port",
                str(port),
                "--telegram=false",
            ],
            cwd=REPO,
        )
        if proc.returncode != 0:
            fail(f"init failed:\n{proc.stdout}{proc.stderr}")
            return finish()
        agent_dirs = list((project / "agents").glob("*"))
        if len(agent_dirs) != 1:
            fail(f"init produced {len(agent_dirs)} agent dirs, want 1")
            return finish()
        agent_dir = agent_dirs[0]
        key = agent_dir.name
        # Pin the compose engine to the harness engine — the mixed-engine
        # split makes every state check lie (same as the full suite).
        with (project / "fleet.yaml").open("a", encoding="utf-8") as f:
            f.write(f"\ndefaults:\n  compose:\n    engine: {ENGINE}\n")
        proc = run([str(agentctl), "secrets", "init"], cwd=agent_dir)
        if proc.returncode != 0:
            fail(f"secrets init failed:\n{proc.stdout}{proc.stderr}")
            return finish()

        stage("fleet deploy --all")
        proc = run([str(agentctl), "fleet", "deploy", "--all"], cwd=project)
        url = f"http://127.0.0.1:{port}/healthz"
        if proc.returncode == 0 and healthy(url, 420):
            pass_("fleet deploy --all: gateway healthy")
        else:
            fail(f"deploy/health failed:\n{proc.stdout}{proc.stderr}")
            return finish()

        stage("fleet serve API")
        serve = subprocess.Popen(
            [str(agentctl), "fleet", "serve", "--port", str(port + 1)],
            cwd=project,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        try:
            token_path = serve_token_path(project)
            deadline = time.monotonic() + 30
            token = ""
            while time.monotonic() < deadline:
                if token_path.exists():
                    token = token_path.read_text(encoding="utf-8").strip()
                    if token:
                        break
                time.sleep(0.5)
            req = urllib.request.Request(f"http://127.0.0.1:{port + 1}/api/v1/roster")
            req.add_header("Authorization", f"Bearer {token}")
            ok = False
            try:
                with urllib.request.urlopen(req, timeout=10) as resp:
                    body = json.load(resp)
                    ok = resp.status == 200 and len(body.get("agents", [])) == 1
            except Exception as exc:
                print(f"  roster error: {exc}", file=sys.stderr)
            if ok:
                pass_("fleet serve: roster answers with the bearer")
            else:
                fail("fleet serve roster did not answer")
        finally:
            serve.terminate()
            serve.wait(timeout=15)

        stage("destroy")
        proc = run([str(agentctl), "destroy", "--volumes", "--yes"], cwd=agent_dir)
        if proc.returncode != 0:
            fail(f"destroy failed:\n{proc.stdout}{proc.stderr}")
        gone = container_gone(key)
        if gone:
            pass_("destroy removed the stack")
        else:
            fail("containers survive destroy")
    finally:
        run([str(agentctl), "destroy", "--volumes", "--yes"], cwd=agent_dir)
    return finish()


def finish() -> int:
    if FAILURES:
        print(f"[e2e] front-door: {FAILURES} failure(s)", file=sys.stderr)
        return 1
    print(f"[e2e] front-door: all green (+{time.monotonic() - T0:.0f}s)")
    return 0


def tempfile_bin() -> str:
    return "/tmp"


def serve_token_path(project: Path) -> Path:
    import hashlib

    state = os.environ.get("XDG_STATE_HOME") or os.path.expanduser("~/.local/state")
    h = hashlib.sha256(str(project).encode()).hexdigest()[:12]
    return Path(state) / "agentctl" / h / "serve-token"


def container_gone(key: str) -> bool:
    argv = [ENGINE, "ps", "-a", "--format", "{{.Names}}"]
    out = subprocess.run(argv, capture_output=True, text=True).stdout
    return f"{key}_agent" not in out and f"{key}-agent" not in out


if __name__ == "__main__":
    sys.exit(main())
