#!/usr/bin/env python3
"""agentctl end-to-end test: the seam hermetic tests cannot cover —
scaffolded artifacts + deploy argv + a real engine + the real image,
together.

Builds the agentctl binary and a local agent-base:e2e image, scaffolds a
throwaway project (implausible base tag 2000.01.01, telegram off, random
gateway port),
then walks the front door: doctor → deploy → health → status/logs →
idempotent redeploy → stop/start → destroy (volume kept, then gone) →
dev overlay. Any failure keeps the full command log under logs/.

The scaffolded stack includes the LiteLLM sidecar (the scaffold default):
the litellm assertions cover the real proxy container — healthcheck
reaching healthy, the seeded baseUrl inside openclaw.json, and the
auth-enforced /v1/models reachable from the agent over model-net.
E2E_LITELLM=0 skips those assertions (offline local runs); the sidecar
itself still deploys as part of the stack.

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
# The scaffold's pinned proxy image; pre-pulled so deploy-time network
# hiccups cannot masquerade as stack failures.
LITELLM_IMAGE = "ghcr.io/berriai/litellm:v1.100.0"
LITELLM_HEALTH_TIMEOUT = int(os.environ.get("E2E_LITELLM_HEALTH_TIMEOUT", "360"))
LITELLM_ENABLED = os.environ.get("E2E_LITELLM", "1") != "0"

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
        if "litellm" in name:
            # No node runtime in the proxy image; its logs above are the
            # diagnostics.
            continue
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


def read_env_value(project: Path, rel: str, name: str) -> str:
    """First NAME= value from a project env file. Values stay in the
    harness process — never printed, never transcribed."""
    try:
        for line in (project / rel).read_text(encoding="utf-8").splitlines():
            if line.startswith(name + "="):
                return line.split("=", 1)[1].strip()
    except OSError:
        pass
    return ""


def container_state(project_name: str, service: str = "agent") -> str | None:
    """State of a stack container under either naming scheme —
    docker compose v2 uses <project>-<svc>-1, the external
    podman-compose provider uses <project>_<svc>_1."""
    proc = engine(["ps", "-a", "--format", "{{.Names}} {{.State}}"])
    names = [f"{project_name}-{service}-1", f"{project_name}_{service}_1"]
    for line in proc.stdout.splitlines():
        parts = line.split()
        if parts and parts[0] in names:
            return parts[1] if len(parts) > 1 else ""
    return None


def litellm_container_name(project_name: str) -> str:
    ps = engine(["ps", "--format", "{{.Names}}"])
    for line in ps.stdout.splitlines():
        if line.strip() in (f"{project_name}-litellm-1", f"{project_name}_litellm_1"):
            return line.strip()
    return ""


def litellm_wait_healthy(project_name: str) -> bool:
    """The sidecar's own healthcheck (python3 → /health/liveliness) is
    the proxy's contract truth; wait for the engine to report healthy."""
    deadline = time.monotonic() + LITELLM_HEALTH_TIMEOUT
    while time.monotonic() < deadline:
        name = litellm_container_name(project_name)
        if name:
            status = engine(["inspect", "-f", "{{.State.Health.Status}}", name])
            if status.stdout.strip() == "healthy":
                return True
        time.sleep(5)
    TRANSCRIPT.append("[diag] litellm container never reached healthy\n")
    return False


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
        client_key = read_env_value(project, "agent/.env", "LITELLM_API_KEY")
        master_key = read_env_value(project, "litellm/.env", "LITELLM_MASTER_KEY")
        if client_key.startswith("sk-") and master_key == client_key:
            pass_("secrets init generated a mirrored sk- master/client key pair")
        else:
            fail("secrets init did not mirror LITELLM_API_KEY / LITELLM_MASTER_KEY")
        mode = (project / "litellm" / ".env").stat().st_mode & 0o777
        if mode == 0o600:
            pass_("litellm/.env is 0600")
        else:
            fail(f"litellm/.env mode is {oct(mode)}, want 0600")

        if LITELLM_ENABLED:
            print(f"[e2e] pre-pulling {LITELLM_IMAGE}")
            for _ in range(3):
                if engine(["pull", LITELLM_IMAGE]).returncode == 0:
                    break
                time.sleep(10)
            else:
                fail(f"could not pre-pull {LITELLM_IMAGE} after 3 tries")

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

        if LITELLM_ENABLED:
            if litellm_wait_healthy(project_name):
                pass_("litellm sidecar reached healthy (/health/liveliness)")
            else:
                fail("litellm sidecar never became healthy")
            agent_name = agent_container_name()
            cfg = engine(["exec", agent_name, "cat", "/home/node/.openclaw/openclaw.json"])
            if "http://litellm:4000" in cfg.stdout:
                pass_("openclaw.json carries the seeded sidecar baseUrl")
            else:
                fail("seeded models.providers.litellm.baseUrl missing from openclaw.json")
            # model-net reachability + auth enforcement: /v1/models must
            # answer 401 (not connection-refused, not 200) without the key.
            probe = (
                "import sys, urllib.request, urllib.error\n"
                "try:\n"
                "    urllib.request.urlopen('http://litellm:4000/v1/models', timeout=5)\n"
                "    sys.exit(1)\n"
                "except urllib.error.HTTPError as e:\n"
                "    sys.exit(0 if e.code == 401 else 1)\n"
            )
            if engine(["exec", agent_name, "python3", "-c", probe]).returncode == 0:
                pass_("proxy /v1/models reachable from the agent and 401s without auth")
            else:
                fail("proxy probe failed (reachability or the 401 contract)")

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
        if (
            container_state(project_name) is None
            and container_state(project_name, "litellm") is None
        ):
            pass_("destroy removed the containers (agent + litellm)")
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
        dev_healthy = proc.returncode == 0 and wait_healthy(port, token)
        if LITELLM_ENABLED:
            dev_healthy = dev_healthy and litellm_wait_healthy(project_name)
        if dev_healthy:
            pass_("dev up boots the overlay stack to healthy")
        else:
            fail(f"dev up failed:\n{indent(proc.stdout + proc.stderr)}")
        agentctl_cmd("dev", "down")
        if (
            container_state(project_name) is None
            and container_state(project_name, "litellm") is None
        ):
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
