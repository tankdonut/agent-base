#!/usr/bin/env python3
"""agentctl end-to-end test: the seam hermetic tests cannot cover —
scaffolded artifacts + deploy argv + a real engine + the real image,
together.

Builds the agentctl binary and a local agent-base:e2e image, scaffolds a
throwaway project (implausible base tag 2000.01.01, telegram off, random
gateway port),
then walks the front door: doctor → deploy → health → status/logs →
idempotent redeploy → stop/start → destroy (volume kept, then gone) →
dev overlay boot. Any failure keeps the full command log under logs/.

Engine: E2E_ENGINE pins it (CI sets docker); default is podman when
available, docker otherwise. The base image is built once as
ghcr.io/tankdonut/agent-base:e2e and reused across runs.
"""

import os
import random
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
LOGDIR = REPO_ROOT / "logs"
BASE_IMAGE = "ghcr.io/tankdonut/agent-base:2000.01.01"
ENGINE = os.environ.get("E2E_ENGINE") or (shutil.which("podman") and "podman") or "docker"
HEALTH_TIMEOUT = int(os.environ.get("E2E_HEALTH_TIMEOUT", "360"))

FAILURES = 0
TRANSCRIPT: list[str] = []


def pass_(msg: str) -> None:
    print(f"  PASS {msg}")


def fail(msg: str) -> None:
    global FAILURES
    print(f"  FAIL {msg}", file=sys.stderr)
    FAILURES += 1


def record(argv: list[str], proc: subprocess.CompletedProcess) -> None:
    TRANSCRIPT.append(
        f"$ {' '.join(str(a) for a in argv)}\n  exit={proc.returncode}\n{proc.stdout}{proc.stderr}"
    )


def run(
    argv: list[str], cwd: Path | None = None, check: bool = False
) -> subprocess.CompletedProcess:
    proc = subprocess.run([str(a) for a in argv], cwd=cwd, capture_output=True, text=True)
    record(argv, proc)
    if check and proc.returncode != 0:
        raise RuntimeError(
            f"command failed ({proc.returncode}): {' '.join(str(a) for a in argv)}\n"
            f"{proc.stdout}{proc.stderr}"
        )
    return proc


def engine(argv: list[str], check: bool = False) -> subprocess.CompletedProcess:
    return run([ENGINE, *argv], check=check)


def in_container_healthy(name: str) -> bool:
    """The contract-level truth: the gateway serves /healthz inside the
    container, reached via engine exec — independent of host port
    forwarding (rootless podman + the external podman-compose provider
    can reset host-side connections while the agent is fully healthy)."""
    probe = (
        "fetch('http://localhost:18789/healthz')"
        ".then(r=>{console.log('healthz',r.status);process.exit(0)})"
        ".catch(e=>{console.log('healthz-fail',e.message);process.exit(1)})"
    )
    return engine(["exec", name, "node", "-e", probe]).returncode == 0


def agent_container_name() -> str:
    ps = engine(["ps", "--format", "{{.Names}}"])
    for line in ps.stdout.splitlines():
        if line.strip() in ("e2e-agent-agent-1", "e2e-agent_agent_1"):
            return line.strip()
    return ""


def wait_healthy(port: int, token: str = "") -> bool:
    """Wait for the agent to be healthy: the in-container gateway first
    (contract truth), then the host port — asserted under docker, where
    port publishing is part of the platform promise; under podman the
    host result is recorded but not fatal (provider-level quirk)."""
    deadline = time.monotonic() + HEALTH_TIMEOUT
    url = f"http://127.0.0.1:{port}/healthz"
    while time.monotonic() < deadline:
        name = agent_container_name()
        if name and in_container_healthy(name):
            break
        time.sleep(3)
    else:
        TRANSCRIPT.append("[diag] in-container healthz never returned 200\n")
        dump_container_diag()
        return False
    return host_probe(url, token)


def host_probe(url: str, token: str) -> bool:
    deadline = time.monotonic() + 60
    last_error = "connection never attempted"
    while time.monotonic() < deadline:
        req = urllib.request.Request(url)
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        try:
            with urllib.request.urlopen(req, timeout=2) as resp:
                if resp.status == 200:
                    return True
                last_error = f"HTTP {resp.status}"
        except urllib.error.HTTPError as e:
            last_error = f"HTTP {e.code}"
        except Exception as e:
            last_error = str(e)
        time.sleep(3)
    TRANSCRIPT.append(f"[diag] healthz {url}: {last_error}\n")
    dump_container_diag()
    return False


def dump_container_diag() -> None:
    ps = engine(["ps", "-a", "--format", "{{.Names}} {{.Status}}"])
    TRANSCRIPT.append(f"[diag] engine ps -a:\n{ps.stdout}")
    for line in ps.stdout.splitlines():
        name = line.split()[0] if line.split() else ""
        if "agent" not in name:
            continue
        logs = engine(["logs", "--tail", "40", name])
        TRANSCRIPT.append(f"[diag] logs {name}:\n{logs.stdout}{logs.stderr}")
        probe = (
            "fetch('http://localhost:18789/healthz')"
            ".then(r=>{console.log('healthz',r.status);process.exit(0)})"
            ".catch(e=>{console.log('healthz-fail',e.message);process.exit(1)})"
        )
        in_container = engine(["exec", name, "node", "-e", probe])
        TRANSCRIPT.append(
            f"[diag] in-container healthz {name}:\n{in_container.stdout}{in_container.stderr}"
        )
        ports = engine(["port", name])
        TRANSCRIPT.append(f"[diag] port mapping {name}:\n{ports.stdout}{ports.stderr}")


def read_gateway_token(project: Path) -> str:
    try:
        for line in (project / "agent" / ".env").read_text(encoding="utf-8").splitlines():
            if line.startswith("OPENCLAW_GATEWAY_TOKEN="):
                return line.split("=", 1)[1].strip()
    except OSError:
        pass
    return ""


def container_state(project_name: str) -> str | None:
    """State of the agent container under either naming scheme —
    docker compose v2 uses <project>-agent-1, the external
    podman-compose provider uses <project>_agent_1."""
    proc = engine(["ps", "-a", "--format", "{{.Names}} {{.State}}"])
    names = [f"{project_name}-agent-1", f"{project_name}_agent_1"]
    for line in proc.stdout.splitlines():
        parts = line.split()
        if parts and parts[0] in names:
            return parts[1] if len(parts) > 1 else ""
    return None


def volume_exists(name: str) -> bool:
    proc = engine(["volume", "ls", "--format", "{{.Name}}"])
    return name in proc.stdout.splitlines()


def main() -> int:
    LOGDIR.mkdir(parents=True, exist_ok=True)
    workdir = Path(tempfile.mkdtemp(prefix="agentctl-e2e-"))
    project = workdir / "e2e-agent"
    port = random.randint(21000, 21999)
    agentctl = workdir / "agentctl"
    project_name = "e2e-agent"

    try:
        print(f"[e2e] engine: {ENGINE}; project: {project}; gateway port: {port}")
        print("[e2e] building agentctl binary")
        run(["go", "build", "-o", agentctl, "./cmd/agentctl"], cwd=REPO_ROOT, check=True)

        have_image = engine(["image", "inspect", BASE_IMAGE]).returncode == 0
        if not have_image:
            print(f"[e2e] building {BASE_IMAGE}")
            fmt = ["--format", "docker"] if ENGINE == "podman" else []
            engine(
                [
                    "build",
                    *fmt,
                    "--build-arg",
                    "AGENT_BASE_VERSION=e2e",
                    "-f",
                    "container/Dockerfile",
                    "-t",
                    BASE_IMAGE,
                    REPO_ROOT,
                ],
                check=True,
            )
        else:
            print(f"[e2e] reusing local {BASE_IMAGE}")

        def agentctl_cmd(*args: str, check: bool = False) -> subprocess.CompletedProcess:
            return run([agentctl, *args], cwd=project, check=check)

        print("[e2e] scaffolding project")
        run(
            [
                agentctl,
                "init",
                str(project),
                "--base-tag",
                "2000.01.01",
                "--gateway-port",
                str(port),
                "--telegram=false",
            ],
            cwd=REPO_ROOT,
            check=True,
        )

        # Pin the project's compose engine to the harness engine: CI
        # runners preinstall podman, and agentctl's auto-detect would
        # build the stack there while these assertions drive E2E_ENGINE
        # — the mixed-engine split makes every state check lie.
        with (project / ".agentctl.yaml").open("a", encoding="utf-8") as f:
            f.write(f"\ncompose:\n  engine: {ENGINE}\n")

        agentctl_cmd("secrets", "init", check=True)
        env_file = project / "agent" / ".env"
        with env_file.open("a", encoding="utf-8") as f:
            f.write("FALLBACK_MODEL=zai/glm-4.7\nZAI_API_KEY=e2e-dummy-key\n")

        print("[e2e] doctor")
        proc = agentctl_cmd("doctor")
        if proc.returncode == 0:
            pass_("doctor passes on a fresh scaffold")
        else:
            fail(f"doctor failed on the scaffolded project:\n{indent(proc.stdout + proc.stderr)}")

        token = read_gateway_token(project)
        print("[e2e] deploy → healthy")
        proc = agentctl_cmd("deploy")
        if proc.returncode != 0:
            fail(f"deploy failed:\n{indent(proc.stdout + proc.stderr)}")
        elif wait_healthy(port, token):
            pass_("deployed agent reached healthy (/healthz 200)")
        else:
            fail(f"agent never became healthy on 127.0.0.1:{port}")

        proc = agentctl_cmd("deploy")
        if proc.returncode == 0:
            pass_("redeploy is idempotent (exit 0)")
        else:
            fail(f"second deploy failed:\n{indent(proc.stdout + proc.stderr)}")

        proc = agentctl_cmd("status")
        if proc.returncode == 0 and "agent" in proc.stdout:
            pass_("status lists the running agent")
        else:
            fail(f"status:\n{indent(proc.stdout + proc.stderr)}")

        proc = agentctl_cmd("logs")
        if proc.returncode == 0:
            pass_("logs exits 0")
        else:
            fail(f"logs failed:\n{indent(proc.stdout + proc.stderr)}")

        print("[e2e] stop / start")
        agentctl_cmd("stop")
        state = container_state(project_name)
        if state in ("exited", "stopped"):
            pass_(f"stop paused the container (state={state})")
        else:
            fail(f"container state after stop: {state}")
        agentctl_cmd("start")
        if wait_healthy(port, token):
            pass_("start resumed to healthy")
        else:
            fail("agent did not return to healthy after start")

        print("[e2e] destroy semantics")
        agentctl_cmd("destroy")
        if container_state(project_name) is None:
            pass_("destroy removed the containers")
        else:
            fail("containers survive destroy")
        data_vol = f"{project_name}_agent-data"
        if volume_exists(data_vol):
            pass_("destroy kept the data volume")
        else:
            fail(f"data volume {data_vol} missing after volume-preserving destroy")

        agentctl_cmd("destroy", "--volumes", "--yes")
        if not volume_exists(data_vol):
            pass_("destroy --volumes removed the data volume")
        else:
            fail("data volume survives --volumes destroy")

        print("[e2e] dev overlay")
        proc = agentctl_cmd("dev", "up")
        if proc.returncode == 0 and wait_healthy(port, token):
            pass_("dev up boots the overlay stack to healthy")
        else:
            fail(f"dev up failed:\n{indent(proc.stdout + proc.stderr)}")
        agentctl_cmd("dev", "down")
        if container_state(project_name) is None:
            pass_("dev down removed the stack")
        else:
            fail("containers survive dev down")
    finally:
        if project.exists() and agentctl.exists():
            run([agentctl, "destroy", "--volumes", "--yes"], cwd=project)
        dump_transcript()

    if FAILURES == 0:
        log = LOGDIR / "agentctl-e2e.log"
        if log.exists():
            log.unlink()
        print("[e2e] PASS (front door opens end to end)")
        return 0
    print(
        f"[e2e] FAIL: {FAILURES} assertion(s); transcript in {LOGDIR}/agentctl-e2e.log",
        file=sys.stderr,
    )
    return 1


def indent(text: str) -> str:
    return "".join(f"    {line}\n" for line in text.splitlines())


def dump_transcript() -> None:
    (LOGDIR / "agentctl-e2e.log").write_text("\n".join(TRANSCRIPT), encoding="utf-8")


if __name__ == "__main__":
    sys.exit(main())
