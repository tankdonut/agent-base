#!/usr/bin/env python3
"""Spec-driven tini entrypoint for the standard-agent base image.

Boot behaviour is declared in /opt/agent/spec.json (loaded by container/
spec.py — fail-closed) and executed as composable phase functions. Projects
with one-off needs do NOT get hooks or plugin loading here: they write a
thin wrapper entrypoint that imports this module, calls the phases they
need, and adds their own logic between them.

First boot (one-time, only when openclaw.json is absent):
  1. openclaw setup --auth-choice {spec.setup.auth_choice} + fallback model
  2. openclaw channels add for each spec.channels entry
  3. openclaw plugins install @openclaw/llama-cpp-provider (always: memory
     search via local embeddings is base-image behaviour)

Every boot (declarative reconciliation — idempotent, self-healing volumes):
  4. reconcile_config   — spec config entries via value-compared config set
  5. reconcile_mcp      — idempotent MCP registration (local + remote)
  6. reconcile_plugins  — local: force-install each boot; registry: install
                          when absent
  7. authenticate_gh    — only when features.gh_auth is true
  8. seed_content       — workspace first-boot-only; skills + docs full
                          replacement every boot
  9. fork; the child runs post_startup (gateway wait, cron seeding via
     seed_automations in-process, memory reindex, skill disable) and the
     parent os.execvp's into the container CMD.

Content-seeding standard (owner decision): docs live at {data}/workspace/
docs — there is no {data}/docs destination. Agents migrating from a legacy
{data}/docs layout (mimir) migrate once in their wrapper entrypoint before
calling seed_content; this module never writes {data}/docs.

Standard environment contract (replaces the freya/mimir FREYA_*/MIMIR_*
names; TELEGRAM_* are NOT special here — they appear inside project
spec.json files via {env:...} templating, if_env guards, and split_csv):

  AGENT_SPEC_PATH        Override the spec.json location (default
                         /opt/agent/spec.json; used by tests and fixtures).
  AGENT_MANAGE_CONFIG    "0" skips config/mcp/plugin reconciliation for
                         operators who manage config manually (default 1).
  AGENT_SKIP_SEED        "1" skips CONTENT SEEDING ONLY — reconciliation
                         still runs (dev overlays bind-mount the content).
  AGENT_MEMORY_REINDEX   "0" skips the post-startup memory reindex
                         (default 1).
  AGENT_GIT_TOKEN        gh auth token, used only when features.gh_auth.
  AGENT_AUTOMATIONS_DIR  Automations directory; resolved inside
                         seed_automations (default /opt/agent/automations)
                         — deliberately not duplicated here.

Resolved secret values must never reach logs: warnings name config keys,
never their values.

Run tests: python3 -m unittest discover -s container -p "test_entrypoint.py" -v
"""

# allow: SIZE_OK — a port of the 860-line freya entrypoint (plus mimir's
# remote-MCP paths) collapsed to generic spec-driven form; every phase
# function is a documented extension surface for wrapper entrypoints.

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import time
from collections.abc import Mapping
from pathlib import Path

import seed_automations
from spec import Spec, SpecError, load_spec, mcp_to_cli_args, required_env_for_auth_choice

SPEC_PATH = Path("/opt/agent/spec.json")
SEED_BASE = Path("/opt/seed")

# Do NOT set OPENCLAW_HOME: OpenClaw treats it as a home dir and appends
# .openclaw/ within it (double-nesting). The default (~/.openclaw) is correct.
os.environ.pop("OPENCLAW_HOME", None)


def log(msg: str) -> None:
    print(f"[agent-entry] {msg}", flush=True)


def warn(msg: str) -> None:
    print(f"[agent-entry] WARNING: {msg}", file=sys.stderr, flush=True)


def run(
    *args: str, check: bool = True, capture: bool = False
) -> subprocess.CompletedProcess[str] | None:
    """Spawn a CLI command; check=True propagates failure, else the failed
    result (or exception object) is returned for the caller to inspect."""
    try:
        return subprocess.run(
            list(args),
            check=check,
            capture_output=capture,
            text=True,
        )
    except subprocess.CalledProcessError as exc:
        if check:
            raise
        return exc


def data_dir() -> Path:
    """The {data} root (~/.openclaw). Resolved per call — never cached — so
    HOME overrides (tests, fixtures) hold for the whole boot."""
    return Path.home() / ".openclaw"


def load_agent_spec(env: Mapping[str, str]) -> Spec:
    """Load the agent spec from SPEC_PATH, or AGENT_SPEC_PATH when set.

    Invariant: env is the only environment consulted; any violation raises
    SpecError and aborts the boot loudly (the loader is fail-closed)."""
    path = Path(env.get("AGENT_SPEC_PATH") or SPEC_PATH)
    return load_spec(path, env)


# --- config reconciliation (value-compared fast path, ported from freya) ---

_MISSING = object()

config_reconcile_stats: dict[str, int] = {"applied": 0, "skipped": 0}


def read_openclaw_config() -> dict | None:
    """Current openclaw.json as a dict, or None when absent/unparseable.

    Re-read on every config_set: mid-reconcile CLI calls (mcp add,
    plugins install) mutate the file, so a cached snapshot could wrongly
    skip a set that is actually needed."""
    try:
        data = json.loads((data_dir() / "openclaw.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    return data if isinstance(data, dict) else None


def lookup_config_path(config: dict, dotted: str) -> object:
    """Resolve a dotted config key ("a.b.c") to its stored value.

    Returns _MISSING when any path segment is absent or not an object —
    the caller then falls back to the CLI."""
    node: object = config
    for part in dotted.split("."):
        if not isinstance(node, dict) or part not in node:
            return _MISSING
        node = node[part]
    return node


def config_value_matches(current: object, desired: object, raw: str) -> bool:
    """True when the stored value already equals what config set writes.

    Exact match first; then the JSON-coerced form (the CLI may store
    ``"true"`` as boolean true, ``"20"`` as number 20). The bool guards
    keep True == 1 from producing a false match. Anything unrecognized
    returns False, so the caller falls back to the CLI — never a silent
    skip on an ambiguous shape."""
    if current == desired and isinstance(current, bool) == isinstance(desired, bool):
        return True
    try:
        coerced = json.loads(raw)
    except json.JSONDecodeError:
        return False
    return current == coerced and isinstance(current, bool) == isinstance(coerced, bool)


def config_set(key: str, value: str, *extra: str) -> None:
    """Set a config key, skipping the CLI call when already reconciled.

    ``openclaw config set`` spawns the Node CLI (~seconds per call); on a
    warm volume every reconcile value is already on disk. Comparing against
    openclaw.json directly is effectively free, so only shell out for keys
    that actually drift. Failure warns (naming the key only — values may
    carry secrets) and never raises."""
    desired: object = value
    if "--strict-json" in extra:
        try:
            desired = json.loads(value)
        except json.JSONDecodeError:
            desired = _MISSING

    if desired is not _MISSING:
        config = read_openclaw_config()
        if config is not None:
            current = lookup_config_path(config, key)
            if config_value_matches(current, desired, value):
                config_reconcile_stats["skipped"] += 1
                return

    config_reconcile_stats["applied"] += 1
    result = run("openclaw", "config", "set", key, value, *extra, check=False)
    if result is not None and result.returncode != 0:
        warn(f"config set failed: {key}")


def guard_satisfied(if_env: tuple[str, ...], env: Mapping[str, str]) -> bool:
    """True iff every name in if_env is present in env; an empty guard
    always passes (mirrors ConfigEntry.env_guard_satisfied)."""
    return all(name in env for name in if_env)


# --- phases (each independently callable from a wrapper entrypoint) ---


def first_boot_setup(spec: Spec) -> None:
    """One-time infrastructure setup; the caller gates this on openclaw.json
    being absent. Invariant: every call here is idempotent-safe to re-run
    but is only invoked on true first boots. A failed setup aborts the boot
    cleanly (exit 1, named env var) — setup leaves no openclaw.json behind,
    so propagating the exception would crash-loop the container."""
    log(f"First boot — configuring OpenClaw (auth: {spec.auth_choice})")
    try:
        run(
            "openclaw",
            "setup",
            "--non-interactive",
            "--accept-risk",
            "--auth-choice",
            spec.auth_choice,
            "--skip-channels",
            "--skip-skills",
            "--skip-daemon",
            "--skip-ui",
            "--skip-health",
            "--skip-search",
        )
    except subprocess.CalledProcessError as exc:
        warn(f"first-boot setup failed (exit {exc.returncode}) — aborting boot")
        required_env = required_env_for_auth_choice(spec.auth_choice)
        if required_env is not None:
            warn(
                f"auth '{spec.auth_choice}' needs {required_env}: "
                "verify it holds a valid key, then restart"
            )
        raise SystemExit(1) from exc

    log(f"Adding {spec.model_fallback} fallback")
    run("openclaw", "models", "fallbacks", "add", spec.model_fallback)

    for channel in spec.channels:
        log(f"Installing {channel.type} channel adapter")
        args = ["openclaw", "channels", "add", "--channel", channel.type]
        if channel.use_env:
            args.append("--use-env")
        run(*args)

    log("Installing local embedding provider (key-free semantic search)")
    run("openclaw", "plugins", "install", "@openclaw/llama-cpp-provider")

    log("Infrastructure setup complete (MCP + plugins reconciled every boot)")


def reconcile_config(spec: Spec, env: Mapping[str, str]) -> None:
    """Apply spec config entries in spec order via config_set. Entries whose
    if_env guard is unsatisfied are skipped with a log line (never an
    error). Resets and reports the applied/skipped counters."""
    log("Reconciling config")
    config_reconcile_stats["applied"] = 0
    config_reconcile_stats["skipped"] = 0

    for entry in spec.config_entries:
        if not entry.env_guard_satisfied(env):
            log(f"skipped (if_env unsatisfied: {entry.path})")
            continue
        if entry.use_strict_json:
            config_set(entry.path, entry.cli_value, "--strict-json")
        else:
            config_set(entry.path, entry.cli_value)

    applied = config_reconcile_stats["applied"]
    skipped = config_reconcile_stats["skipped"]
    log(f"Config reconcile: {applied} set, {skipped} already current")


def mcp_exists(name: str) -> bool:
    """True iff `openclaw mcp list --json` output contains the name. Any CLI
    failure counts as absent (the subsequent add self-heals)."""
    result = run("openclaw", "mcp", "list", "--json", check=False, capture=True)
    if result is None or result.returncode != 0:
        return False
    return f'"{name}"' in result.stdout


def reconcile_mcp(spec: Spec, env: Mapping[str, str]) -> None:
    """Register each spec MCP server when missing (flags come from
    mcp_to_cli_args; --no-probe is included by that builder). Servers whose
    if_env guard is unsatisfied are skipped with a log line. Failures warn
    and never raise."""
    for server in spec.mcp_servers:
        if not guard_satisfied(server.if_env, env):
            log(f"MCP server '{server.name}' skipped (if_env unsatisfied)")
            continue
        if mcp_exists(server.name):
            log(f"MCP server '{server.name}' already registered — skipping")
            continue
        log(f"Registering MCP server '{server.name}'")
        result = run("openclaw", "mcp", "add", server.name, *mcp_to_cli_args(server), check=False)
        if result is not None and result.returncode != 0:
            warn(f"mcp add failed: {server.name}")


def plugin_exists(name: str) -> bool:
    """True iff `openclaw plugins list --json` output contains the name. Any
    CLI failure counts as absent."""
    result = run("openclaw", "plugins", "list", "--json", check=False, capture=True)
    if result is None or result.returncode != 0:
        return False
    return f'"{name}"' in result.stdout


def reconcile_plugins(spec: Spec) -> None:
    """Local-source plugins (absolute path) are force-installed on every
    boot — their content is image-baked and must track the image. Registry
    plugins install only when absent. Failures warn and never raise."""
    for plugin in spec.plugins:
        if plugin.source is not None:
            log(f"Reconciling local plugin '{plugin.name}'")
            run("openclaw", "plugins", "install", plugin.source, "--force", check=False)
        elif not plugin_exists(plugin.name):
            log(f"Installing plugin '{plugin.name}'")
            result = run("openclaw", "plugins", "install", plugin.name, check=False)
            if result is not None and result.returncode != 0:
                warn(f"plugin install failed: {plugin.name}")
        else:
            log(f"Plugin '{plugin.name}' already installed — skipping")


def authenticate_gh(env: Mapping[str, str]) -> None:
    """Authenticate the GitHub CLI (gh) from AGENT_GIT_TOKEN.

    gh auth state (~/.config/gh/) lives outside the data volume, so it is
    re-established every boot when features.gh_auth is enabled. Non-fatal:
    on failure callers fall back to per-invocation auth. The token itself
    is never logged."""
    token = env.get("AGENT_GIT_TOKEN", "")
    if not token:
        warn("AGENT_GIT_TOKEN not set — gh not authenticated")
        return

    if not shutil.which("gh"):
        warn("gh CLI not on PATH — skipping auth")
        return

    try:
        result = subprocess.run(
            ["gh", "auth", "login", "--with-token"],
            input=token,
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )
    except subprocess.TimeoutExpired:
        warn("gh auth login timed out — gh not authenticated")
        return

    if result.returncode == 0:
        log("Authenticated gh CLI from AGENT_GIT_TOKEN")
    else:
        stderr = (result.stderr or "").strip()
        warn(f"gh auth login failed (exit {result.returncode}) — gh not authenticated")
        if stderr:
            warn(f"gh auth stderr: {stderr}")


def seed_content(spec: Spec, env: Mapping[str, str]) -> None:
    """Seed content from SEED_BASE subdirs into {data}.

    workspace/ is copied on first boot only (the agent evolves it at
    runtime); skills/ and docs/ are fully replaced every boot (image-baked
    reference content; docs land at {data}/workspace/docs). journal/ is
    created with parents. AGENT_SKIP_SEED=1 skips content seeding only —
    config/mcp/plugin reconciliation is the caller's business and still
    runs."""
    if env.get("AGENT_SKIP_SEED", "0") == "1":
        log("Content seeding skipped (AGENT_SKIP_SEED=1 — dev mode bind mounts)")
        return

    workspace = data_dir() / "workspace"
    workspace_src = SEED_BASE / "workspace"
    if not workspace.exists() and workspace_src.is_dir():
        shutil.copytree(workspace_src, workspace)
    else:
        workspace.mkdir(parents=True, exist_ok=True)

    skills_src = SEED_BASE / "skills"
    if skills_src.is_dir():
        skills_dst = data_dir() / "skills"
        if skills_dst.exists():
            shutil.rmtree(skills_dst)
        shutil.copytree(skills_src, skills_dst)

    docs_src = SEED_BASE / "docs"
    if docs_src.is_dir():
        docs_dst = workspace / "docs"
        if docs_dst.exists():
            shutil.rmtree(docs_dst)
        shutil.copytree(docs_src, docs_dst)

    (workspace / "journal").mkdir(parents=True, exist_ok=True)

    log(f"Seeded workspace, skills, and docs for {spec.agent_name}")


# --- post-startup (forked child; ported from freya) ---


def wait_for_gateway(timeout_s: int = 180) -> bool:
    """Poll `openclaw health` every 2s until healthy or the deadline
    passes. Returns False on timeout — the caller skips post-startup."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        result = run("openclaw", "health", check=False, capture=True)
        if result is not None and result.returncode == 0:
            return True
        time.sleep(2)
    return False


def check_memory_status(agent: str = "main") -> str:
    """Check whether the memory index needs reindexing.

    Returns 'force' (full rebuild — model changed or index missing),
    'incremental' (content changed since last index), or 'skip' (clean).
    On any error, defaults to 'force' as a safe fallback."""
    try:
        result = subprocess.run(
            ["openclaw", "memory", "status", "--agent", agent, "--json"],
            check=False,
            capture_output=True,
            text=True,
            timeout=60,
        )
    except subprocess.TimeoutExpired:
        warn("memory status check timed out — defaulting to force reindex")
        return "force"

    if result.returncode != 0:
        warn(f"memory status check failed (exit {result.returncode}) — defaulting to force reindex")
        return "force"

    try:
        payloads = json.loads(result.stdout)
        status = payloads[0]["status"]
    except (json.JSONDecodeError, IndexError, KeyError, TypeError):
        warn("memory status: unparseable JSON — defaulting to force reindex")
        return "force"

    identity = status.get("custom", {}).get("indexIdentity", {})
    identity_status = identity.get("status", "valid")

    if identity_status in ("mismatched", "missing"):
        reason = identity.get("reason", identity_status)
        log(f"memory index identity: {identity_status} ({reason}) — full rebuild needed")
        return "force"

    if status.get("dirty"):
        log("memory index is dirty — incremental reindex needed")
        return "incremental"

    files = status.get("files", 0)
    log(f"memory index is clean ({files} files indexed) — skipping reindex")
    return "skip"


def reindex_memory(
    agent: str = "main", force: bool = True, attempts: int = 3, backoff_s: int = 10
) -> None:
    """Memory reindex with retry. The embedding provider loads during the sync
    call, so the first attempt may fail before the plugin is ready — retry
    until it comes up. A degraded success (vectors skipped, FTS-only) exits 0
    but emits ``chunks_vec not updated`` on stderr; detect it and treat as
    retryable, since it is the memory-search-offline case.

    force=True for a full rebuild (model change, missing index); force=False
    for incremental (only re-embed changed files)."""
    cmd = ["openclaw", "memory", "index", "--agent", agent, "--verbose"]
    if force:
        cmd.insert(2, "--force")
    mode = "full" if force else "incremental"

    for attempt in range(1, attempts + 1):
        log(f"memory reindex: starting attempt {attempt}/{attempts} ({mode})")
        try:
            result = subprocess.run(
                cmd,
                check=False,
                capture_output=True,
                text=True,
                timeout=600,
            )
        except subprocess.TimeoutExpired:
            warn(f"memory reindex attempt {attempt}/{attempts} timed out after 600s")
            if attempt < attempts:
                time.sleep(backoff_s)
            continue

        stdout = (result.stdout or "").strip()
        stderr = (result.stderr or "").strip()
        degraded = "chunks_vec not updated" in stderr

        if result.returncode == 0 and not degraded:
            log(f"memory reindex succeeded (attempt {attempt}/{attempts})")
            if stdout:
                log(f"memory index stdout: {stdout}")
            return

        if degraded:
            warn(
                f"memory reindex attempt {attempt}/{attempts} degraded — "
                "vectors skipped (FTS-only recall)"
            )
        else:
            warn(f"memory reindex attempt {attempt}/{attempts} failed (exit {result.returncode})")
        if stderr:
            warn(f"memory index stderr: {stderr}")
        if stdout:
            warn(f"memory index stdout: {stdout}")

        if attempt < attempts:
            log(f"retrying memory reindex in {backoff_s}s")
            time.sleep(backoff_s)

    warn(
        f"memory reindex failed after {attempts} attempts — "
        "memory_search offline until rebuilt manually"
    )


def disable_unavailable_skills() -> None:
    """Turn off skills flagged by `openclaw doctor --lint` as not ready:
    every skills-readiness finding maps to config_set
    skills.entries.<name>.enabled=false. Never raises."""
    result = run("openclaw", "doctor", "--lint", check=False, capture=True)
    if result is None or result.returncode != 0:
        return
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return

    prefix = "skills.entries."
    suffix = ".enabled"
    for finding in data.get("findings", []):
        if finding.get("checkId") != "core/doctor/skills-readiness":
            continue
        path = finding.get("path", "")
        if path.startswith(prefix) and path.endswith(suffix):
            name = path[len(prefix) : -len(suffix)]
            if name:
                config_set(f"skills.entries.{name}.enabled", "false")


def run_seed_automations(model: str) -> int:
    """Invoke seed_automations.main in-process with the spec's automation
    model. The automations dir (AGENT_AUTOMATIONS_DIR, default
    /opt/agent/automations) is resolved inside that module. Returns the
    would-be exit code (main() signals failure via SystemExit)."""
    try:
        seed_automations.main(["--model", model])
    except SystemExit as exc:
        return exc.code if isinstance(exc.code, int) else 1
    return 0


def post_startup(spec: Spec, env: Mapping[str, str]) -> None:
    """Forked-child phase: everything here needs the gateway up. Waits for
    health, seeds cron jobs, reindexes memory (unless
    AGENT_MEMORY_REINDEX=0), disables unavailable skills, and runs
    warn-only config validation. Never called on the parent path."""
    if not wait_for_gateway():
        warn(
            "gateway did not become healthy within 180s — "
            "skipping post-startup (cron + memory reindex)"
        )
        return

    log("Gateway healthy — running post-startup tasks")

    log("cron seeding: starting")
    cron_exit = run_seed_automations(spec.automations_model)
    if cron_exit != 0:
        warn(f"cron seeding failed (exit {cron_exit}, non-fatal)")
    else:
        log("cron seeding: complete")

    if env.get("AGENT_MEMORY_REINDEX", "1") != "0":
        locks = list((data_dir() / "agents").glob("*/agent/*.reindex-lock.sqlite"))
        if locks:
            log(f"removing {len(locks)} stale reindex lock(s)")
            for lock_file in locks:
                lock_file.unlink(missing_ok=True)
        action = check_memory_status()
        if action != "skip":
            reindex_memory(force=(action == "force"))
        log("memory reindex: complete")
    else:
        log("memory reindex: skipped (AGENT_MEMORY_REINDEX=0)")

    disable_unavailable_skills()

    result = run("openclaw", "config", "validate", check=False, capture=True)
    if result is not None and result.returncode != 0:
        warn("config validation found issues")

    result = run("openclaw", "doctor", "--lint", check=False, capture=True)
    if result is not None and result.returncode != 0:
        warn("doctor lint found issues")


def validate_spec(env: Mapping[str, str]) -> int:
    """--validate-spec mode for downstream CI: load the spec and the
    automations directory WITHOUT any mutation. Returns 0 when both parse,
    1 (reason on stderr) otherwise."""
    try:
        load_agent_spec(env)
        seed_automations.build_jobs()
    except (SpecError, seed_automations.AutomationSpecError, OSError) as exc:
        print(f"[agent-entry] spec validation failed: {exc}", file=sys.stderr, flush=True)
        return 1
    log("spec validation passed")
    return 0


def main(argv: list[str] | None = None) -> int:
    """Boot the agent container and hand off to the container CMD.

    The happy path ends in os.execvp (never returns); an int is returned
    only for --validate-spec (0/1) and usage errors (2)."""
    args = list(sys.argv[1:]) if argv is None else list(argv)
    command = [arg for arg in args if arg != "--validate-spec"]
    env = os.environ

    if "--validate-spec" in args:
        return validate_spec(env)

    if not command:
        warn("no command to hand off to — usage: entrypoint.py [--validate-spec] CMD ARGS...")
        return 2

    spec = load_agent_spec(env)

    data_dir().mkdir(parents=True, exist_ok=True)

    if not (data_dir() / "openclaw.json").exists():
        first_boot_setup(spec)

    if env.get("AGENT_MANAGE_CONFIG", "1") == "1":
        reconcile_config(spec, env)
        reconcile_mcp(spec, env)
        reconcile_plugins(spec)

    if spec.features.gh_auth:
        authenticate_gh(env)

    seed_content(spec, env)

    pid = os.fork()
    if pid == 0:
        post_startup(spec, env)
        os._exit(0)

    log("Scheduled post-startup: cron seeding + memory reindex (background)")

    os.execvp(command[0], command)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
