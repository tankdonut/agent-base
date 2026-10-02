#!/usr/bin/env python3
"""agentctl end-to-end test: the seam hermetic tests cannot cover —
scaffolded artifacts + deploy argv + a real engine + the real image,
together.

Builds the agentctl binary and a local agent-base:e2e image, scaffolds a
throwaway project (implausible base tag 2099.12.31, telegram off, random
gateway port),
then walks the front door: doctor (drift + report) → validate (real-image spec gate,
positive + fail-closed halves) → deploy → health → post-upgrade verify →
the upgrade cycle (gate → backup → rewrite → deploy → verify against a
second bake) → status/logs → idempotent redeploy → stop/start → destroy (volume kept,
then gone) → dev overlay → 2-agent fleet → shared-litellm plane. E2E_STAGES
selects a subset for iteration (plane implies fleet). Any failure keeps the full command log under
logs/.

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

import json
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
# Synthetic dual-bake cycle (defaults): two bakes of one context under
# the implausible sentinel tags — mechanics-drift evidence only, never
# release qualification. E2E_BASE_IMAGE / E2E_UPGRADE_IMAGE point the
# suite at real candidate refs (tag form, agentctl's repo contract
# applies): overridden refs are never baked locally — present or
# pullable or fail — so the run stays true to the demanded bytes.
SYNTH_BASE = "ghcr.io/tankdonut/agent-base:2099.12.31"
SYNTH_UPGRADE = "ghcr.io/tankdonut/agent-base:2099.12.31.1"
BASE_IMAGE = os.environ.get("E2E_BASE_IMAGE", SYNTH_BASE)
# The upgrade target: a second bake of the same context. The running
# image reports its BAKED AGENT_BASE_VERSION, not the registry tag, so
# the dual-tag cycle needs two bakes (cached layers make the second
# cheap) rather than two tags of one image.
UPGRADE_IMAGE = os.environ.get("E2E_UPGRADE_IMAGE", SYNTH_UPGRADE)
ENGINE = os.environ.get("E2E_ENGINE") or (shutil.which("podman") and "podman") or "docker"


def tag_of(ref: str) -> str:
    # agentctl's upgrade verb and init --base-tag speak tags; digest
    # refs have no tag to rewrite to.
    if "@" in ref:
        raise SystemExit(f"E2E base/upgrade image overrides must be tag refs, got: {ref}")
    return ref.rsplit(":", 1)[-1]


BASE_TAG = tag_of(BASE_IMAGE)
UPGRADE_TAG = tag_of(UPGRADE_IMAGE)


def image_env_version(ref: str) -> str | None:
    """The AGENT_BASE_VERSION the image actually bakes (ENV), or None."""
    proc = engine(
        [
            "image",
            "inspect",
            "--format",
            "{{range .Config.Env}}{{println .}}{{end}}",
            ref,
        ]
    )
    for line in proc.stdout.splitlines():
        if line.startswith("AGENT_BASE_VERSION="):
            return line.split("=", 1)[1].strip()
    return None


def ensure_image(ref: str, synth_version: str | None) -> None:
    """Present-or-pullable, with a local bake ONLY for the synthetic
    sentinels — overridden candidate refs get no build fallback."""
    if subprocess.run([ENGINE, "image", "inspect", ref], capture_output=True).returncode == 0:
        print(f"[e2e] reusing local {ref}")
        return
    if synth_version is not None:
        print(f"[e2e] building {ref}")
        fmt = ["--format", "docker"] if ENGINE == "podman" else []
        subprocess.run(
            [
                ENGINE,
                "build",
                *fmt,
                "--build-arg",
                f"AGENT_BASE_VERSION={synth_version}",
                "-f",
                str(REPO_ROOT / "container" / "Dockerfile"),
                "-t",
                ref,
                str(REPO_ROOT),
            ],
            check=True,
        )
        return
    pull = subprocess.run([ENGINE, "pull", ref], capture_output=True, text=True)
    if pull.returncode != 0:
        raise SystemExit(f"image {ref} unavailable and override refs are never built locally")


# Stage selection: the full front door by default; a comma list
# runs a subset for iteration (plane pulls in fleet — it needs
# the two-agent roster).
STAGES = [
    x.strip() for x in os.environ.get("E2E_STAGES", "core,dev,fleet,plane").split(",") if x.strip()
]
if "plane" in STAGES and "fleet" not in STAGES:
    STAGES.append("fleet")


def want(stage: str) -> bool:
    return stage in STAGES


T0 = time.monotonic()


def stage(label: str) -> None:
    """Stage/step banner with elapsed time — every slow run names its
    slow step instead of guessing from a wall-clock total."""
    print(f"[e2e] {label} (+{time.monotonic() - T0:.0f}s)")


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
        for line in (project / ".env").read_text(encoding="utf-8").splitlines():
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

        ensure_image(BASE_IMAGE, "2099.12.31" if BASE_IMAGE == SYNTH_BASE else None)

        def agentctl_cmd(*args: str, check: bool = False) -> subprocess.CompletedProcess:
            return run([agentctl, *args], cwd=project, check=check)

        stage("scaffolding project")
        run(
            [
                agentctl,
                "init",
                str(project),
                "--base-tag",
                BASE_TAG,
                "--gateway-port",
                str(port),
                "--telegram=false",
            ],
            cwd=REPO_ROOT,
            check=True,
        )

        # Pin the fleet's compose engine to the harness engine: CI
        # runners preinstall podman, and agentctl's auto-detect would
        # build the stack there while these assertions drive E2E_ENGINE
        # — the mixed-engine split makes every state check lie. The
        # random gateway port is recorded by init itself (fleet.yaml
        # agents entry), so doctor's drift render recovers the real
        # port, not the default.
        agent = project / "agents" / "e2e-agent"
        with (project / "fleet.yaml").open("a", encoding="utf-8") as f:
            f.write(f"\ndefaults:\n  compose:\n    engine: {ENGINE}\n")

        agentctl_cmd("secrets", "init", check=True)
        client_key = read_env_value(agent, ".env", "LITELLM_API_KEY")
        master_key = read_env_value(agent, "litellm/.env", "LITELLM_MASTER_KEY")
        if client_key.startswith("sk-") and master_key == client_key:
            pass_("secrets init generated a mirrored sk- master/client key pair")
        else:
            fail("secrets init did not mirror LITELLM_API_KEY / LITELLM_MASTER_KEY")
        mode = (agent / "litellm" / ".env").stat().st_mode & 0o777
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

        if want("core"):
            stage("core: doctor")
            proc = agentctl_cmd("doctor")
            if proc.returncode == 0:
                pass_("doctor passes on a fresh scaffold")
            else:
                fail(
                    f"doctor failed on the scaffolded project:\n{indent(proc.stdout + proc.stderr)}"
                )

            # Template drift must be zero on the pristine scaffold, and the
            # --report bundle must carry env KEY names but never values.
            report_path = project / "doctor-report.json"
            proc = agentctl_cmd("doctor", "--report", str(report_path))
            if proc.returncode != 0 or "report written to" not in proc.stdout:
                fail(f"doctor --report failed:\n{indent(proc.stdout + proc.stderr)}")
            pass_("doctor --report wrote the bundle")
            bundle = json.loads(report_path.read_text(encoding="utf-8"))
            drift = [c for c in bundle["checks"] if c["name"].startswith("template/")]
            if len(drift) == 3 and all(c["status"] == "ok" for c in drift):
                pass_("zero template drift on the fresh scaffold")
            else:
                fail(f"expected 3 ok template checks, got: {drift}")
            values = []
            for env_file in (".env", "litellm/.env"):
                for line in (agent / env_file).read_text(encoding="utf-8").splitlines():
                    if "=" in line:
                        value = line.split("=", 1)[1].strip()
                        if value:
                            values.append(value)
            body = report_path.read_text(encoding="utf-8")
            leaked = [v for v in values if v in body]
            if leaked:
                fail(f"doctor --report leaked {len(leaked)} env value(s)")
            pass_("doctor --report carries keys, never values")

            # The base image's real spec gate — 0 on the scaffold, 1 naming
            # the offending JSON path on a broken spec.
            print("[e2e] validate")
            proc = agentctl_cmd("validate")
            if proc.returncode == 0:
                pass_("validate: base image parses spec + automations")
            else:
                fail(f"validate failed:\n{indent(proc.stdout + proc.stderr)}")
            spec_path = agent / "spec.json"
            original_spec = spec_path.read_text(encoding="utf-8")
            spec_path.write_text(
                '{\n  "specVersion": 1,\n  "agent": {"name": "x"},\n  "bogus_key": true\n}',
                encoding="utf-8",
            )
            try:
                proc = agentctl_cmd("validate")
                if proc.returncode == 1 and "bogus_key" in (proc.stdout + proc.stderr):
                    pass_("validate fails closed on a broken spec, naming the JSON path")
                else:
                    fail(
                        f"validate on a broken spec: rc={proc.returncode} (want 1 naming bogus_key)"
                    )
            finally:
                spec_path.write_text(original_spec, encoding="utf-8")
            proc = agentctl_cmd("validate")
            if proc.returncode == 0:
                pass_("validate green again after restoring the spec")
            else:
                fail(f"validate still failing after restore:\n{indent(proc.stdout + proc.stderr)}")

            token = read_gateway_token(agent)
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

            # The upgrade runbook's verify step, commanded. post-startup
            # writes status.json at the end of its child (cron seeding
            # happens before that write in the same child), so polling for
            # the file also gates the cron assertions. The image bakes
            # AGENT_BASE_VERSION=2099.12.31 — equal to the project's pin —
            # so the default expectation (the Dockerfile pin) applies.
            print("[e2e] doctor --post-upgrade")
            deadline = time.monotonic() + 300
            while time.monotonic() < deadline:
                name = agent_container_name()
                if (
                    name
                    and engine(["exec", name, "cat", "/home/node/.openclaw/status.json"]).returncode
                    == 0
                ):
                    break
                time.sleep(3)
            else:
                fail("status.json never appeared — post-startup did not complete")
            proc = agentctl_cmd("doctor", "--post-upgrade")
            if proc.returncode == 0 and "all checks passed" in proc.stdout:
                pass_("doctor --post-upgrade green (marker, backup, mcp, cron, status, heal)")
            else:
                fail(f"doctor --post-upgrade failed:\n{indent(proc.stdout + proc.stderr)}")

            # The upgrade runbook as a verb, against a real warm volume:
            # the doctor --target gate passes (future-dated tags cross zero
            # eras), the image-side backup-before-mutation fires on the
            # version delta, and the pin ends up on the new bake.
            print("[e2e] upgrade")
            ensure_image(UPGRADE_IMAGE, "2099.12.31.1" if UPGRADE_IMAGE == SYNTH_UPGRADE else None)
            upgrade_version = image_env_version(UPGRADE_IMAGE)
            if upgrade_version is None:
                fail(f"upgrade image {UPGRADE_IMAGE} bakes no AGENT_BASE_VERSION")
            proc = agentctl_cmd("upgrade", UPGRADE_TAG, "--yes")
            if proc.returncode == 0 and "post-upgrade verification green" in proc.stdout:
                pass_(f"upgrade {BASE_TAG} → {UPGRADE_TAG} (gate, backup, rewrite, deploy, verify)")
            else:
                fail(f"upgrade failed:\n{indent(proc.stdout + proc.stderr)}")
            from_lines = [
                line
                for line in (agent / "Dockerfile").read_text(encoding="utf-8").splitlines()
                if line.startswith("FROM ")
            ]
            if from_lines and from_lines[0].endswith(f":{UPGRADE_TAG}"):
                pass_("Dockerfile FROM rewritten to the upgrade target")
            else:
                fail(f"FROM line not rewritten: {from_lines}")
            marker = engine(
                ["exec", agent_container_name(), "cat", "/home/node/.openclaw/last-image-version"]
            )
            if upgrade_version is not None and marker.stdout.strip() == upgrade_version:
                pass_("running instance marker reports the new image")
            else:
                fail(
                    f"last-image-version = {marker.stdout.strip()!r}, "
                    f"want baked {UPGRADE_TAG} version {upgrade_version!r}"
                )

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

        if want("dev"):
            stage("dev overlay")
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

        # Shared fleet facts: the second agent's port and both probe
        # URLs (the fleet stage registers the agent; the plane stage
        # reuses the roster either way).
        second_port = random.randint(22000, 22999)
        first_url = f"http://127.0.0.1:{port}/healthz"
        second_url = f"http://127.0.0.1:{second_port}/healthz"
        helper_dir = project / "agents" / "helper"

        if want("fleet"):
            stage("fleet: two agents")
            run(
                [
                    agentctl,
                    "fleet",
                    "add",
                    "helper",
                    "--port",
                    str(second_port),
                    "--base-tag",
                    BASE_TAG,
                    "--telegram=false",
                ],
                cwd=project,
                check=True,
            )
            # fleet add scaffolds the example envs only; the deploy
            # contract needs the per-agent secrets (mirrored sk- key pair).
            helper_dir = project / "agents" / "helper"
            run([agentctl, "secrets", "init"], cwd=helper_dir, check=True)
            proc = run([agentctl, "fleet", "deploy", "--all"], cwd=project)
            if proc.returncode == 0 and (host_probe(second_url, "") and host_probe(first_url, "")):
                pass_("fleet deploy --all brought both agents healthy on distinct ports")
            else:
                fail(f"fleet deploy --all failed:\n{indent(proc.stdout + proc.stderr)}")

            # Batching is never implicit: an unscoped fleet verb must refuse.
            proc = run([agentctl, "fleet", "status"], cwd=project)
            if proc.returncode != 0 and "never implicit" in proc.stdout + proc.stderr:
                pass_("unscoped fleet verb on a 2-agent roster refuses")
            else:
                fail("unscoped fleet verb did not demand --agent/--all")

            # Running-config drift: move the manifest port out from under
            # the live stack, expect the check to flag it, then converge
            # back with a scoped deploy.
            manifest = project / "fleet.yaml"
            before = manifest.read_text(encoding="utf-8")
            manifest.write_text(
                before.replace(f"gateway_port: {second_port}", f"gateway_port: {second_port + 1}"),
                encoding="utf-8",
            )
            proc = run([agentctl, "fleet", "check"], cwd=project)
            body = proc.stdout + proc.stderr
            if "running stack publishes" in body and f"allocates {second_port + 1}" in body:
                pass_("fleet check flags running-port drift after a manifest port edit")
            else:
                fail(f"running-port drift not flagged:\n{indent(body)}")
            manifest.write_text(before, encoding="utf-8")
            run([agentctl, "fleet", "deploy", "--agent", "helper"], cwd=project, check=True)

            run([agentctl, "fleet", "stop", "--all"], cwd=project, check=True)
            run([agentctl, "fleet", "start", "--all"], cwd=project, check=True)
            if host_probe(second_url, ""):
                pass_("fleet stop/start --all round-trips both agents")
            else:
                fail("fleet start --all left an agent unhealthy")

            if not want("plane"):
                run([agentctl, "destroy", "--volumes", "--yes"], cwd=helper_dir, check=True)
                if container_state("helper") is None:
                    pass_("destroy from the agent dir removes the helper stack")
                else:
                    fail("helper container survives destroy")
        if want("plane"):
            stage("plane: shared litellm")
            if not want("fleet"):
                # Minimal roster bootstrap: the second agent without
                # replaying the fleet assertions.
                run(
                    [
                        agentctl,
                        "fleet",
                        "add",
                        "helper",
                        "--port",
                        str(second_port),
                        "--base-tag",
                        BASE_TAG,
                        "--telegram=false",
                    ],
                    cwd=project,
                    check=True,
                )
                run([agentctl, "secrets", "init"], cwd=helper_dir, check=True)
            # Enable the plane (shared proxy, no observability — the
            # observability stack boots with its own phase). Agents with
            # unset litellm inherit the plane's shared placement.
            plane_name = "e2e-plane"
            current = manifest.read_text(encoding="utf-8")
            head = current.split("plane:", 1)[0]
            tail = current[current.index("agents:") :]
            manifest.write_text(
                head
                + "plane:\n"
                + "  enabled: true\n"
                + f"  name: {plane_name}\n"
                + f"  gateway_base_port: {port}\n"
                + "  litellm: shared\n"
                + "  observability: false\n"
                + tail,
                encoding="utf-8",
            )
            # The first render lands the artifacts and (correctly)
            # reports the missing authored plane/.env — write it right
            # after; every later verb re-materializes anyway.
            run([agentctl, "fleet", "render"], cwd=project)
            plane_dir = project / "plane"
            if (plane_dir / "compose.yml").exists() and (
                plane_dir / "litellm" / "config.yaml"
            ).exists():
                pass_("fleet render materialized the plane stack")
            else:
                fail("plane artifacts missing after fleet render")
            plane_env = plane_dir / ".env"
            plane_env.write_text(
                "LITELLM_MASTER_KEY=sk-master-e2e-plane\nPOSTGRES_PASSWORD=e2e-plane-db\n",
                encoding="utf-8",
            )
            # Shared placement retires the agent-local litellm trees (the
            # sidecar mirror check would otherwise fail on virtual keys).
            for key in (project_name, "helper"):
                shutil.rmtree(project / "agents" / key / "litellm", ignore_errors=True)

            stage("plane: up (postgres + litellm cold boot)")
            run([agentctl, "fleet", "plane", "up"], cwd=project, check=True)
            if litellm_wait_healthy(plane_name):
                pass_("plane up brought litellm-db + shared proxy healthy")
            else:
                fail("plane litellm never reached healthy")

            stage("plane: minting virtual keys")
            virtual_keys: dict[str, str] = {}
            for key in (project_name, "helper"):
                run([agentctl, "fleet", "key", key], cwd=project, check=True)
                value = read_env_value(project, f"agents/{key}/.env", "LITELLM_API_KEY")
                if value.startswith("sk-"):
                    virtual_keys[key] = value
                else:
                    fail(f"fleet key did not land a virtual key for {key}")
            if len(set(virtual_keys.values())) == len(virtual_keys) and len(virtual_keys) == 2:
                pass_("both agents hold distinct minted virtual keys")
            else:
                fail("virtual keys are not distinct per agent")
            # Both keys authenticate through the ONE shared proxy (empty
            # model list is fine — this is auth-plane proof, not inference).
            for key, value in virtual_keys.items():
                req = urllib.request.Request("http://127.0.0.1:4000/v1/models")
                req.add_header("Authorization", f"Bearer {value}")
                try:
                    with urllib.request.urlopen(req, timeout=10) as resp:
                        ok_models = resp.status == 200
                except Exception:
                    ok_models = False
                if ok_models:
                    pass_(f"{key}'s virtual key authenticates at the shared proxy")
                else:
                    fail(f"{key}'s virtual key rejected by the shared proxy")

            stage("plane: deploy --all (both agents)")
            run([agentctl, "fleet", "deploy", "--all"], cwd=project, check=True)
            if host_probe(first_url, "") and host_probe(second_url, ""):
                pass_("shared-plane agents deploy healthy (no local sidecars)")
            else:
                fail("shared-plane agents did not reach healthy")

            # Resilience contract: the plane is a shared dependency, not a
            # lifecycle owner — plane down must not take gateways down.
            # (Exit code ignored: engines complain removing a network with
            # agents still attached; the plane containers are gone either
            # way, which is the degraded state under proof.)
            stage("plane: down (resilience check)")
            proc = run([agentctl, "fleet", "plane", "down"], cwd=project)
            if host_probe(first_url, "") and host_probe(second_url, ""):
                pass_("plane down leaves agent gateways healthy")
            else:
                fail(f"plane down degraded agent gateways:\n{indent(proc.stdout + proc.stderr)}")
            run([agentctl, "destroy", "--volumes", "--yes"], cwd=helper_dir, check=True)
            if container_state("helper") is None:
                pass_("destroy from the agent dir removes the helper stack")
            else:
                fail("helper container survives destroy")

    finally:
        if project.exists() and agentctl.exists():
            # Two agents on the roster: the single-agent destroy verb
            # scopes by cwd, so clean each agent from its own directory.
            first_dir = project / "agents" / project_name
            for agent_dir in (first_dir, project / "agents" / "helper"):
                if agent_dir.exists():
                    run(
                        [agentctl, "destroy", "--volumes", "--yes"],
                        cwd=agent_dir,
                    )
            # Plane teardown follows — its network frees once the
            # attached agents are gone.
            run([agentctl, "fleet", "plane", "down", "--volumes"], cwd=project)
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
