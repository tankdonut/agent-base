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
     seed_automations in-process, memory reindex, skill disable) while the
     parent supervise()s the container CMD: the CMD runs in its own
     process group, a shutdown signal is forwarded to the CMD only, and
     in-flight automations drain for up to AGENT_SHUTDOWN_GRACE seconds
     before the group is force-killed and the CMD's exit code returned.

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
  AGENT_SHUTDOWN_GRACE   Seconds a shutdown signal (SIGTERM/SIGINT) waits
                          for the gateway and its in-flight automations
                          before their process group is force-killed
                          (default 600; 0 forwards then force-kills at
                          once).
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

import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import time
from collections.abc import Callable, Mapping
from contextlib import suppress
from datetime import UTC, datetime
from pathlib import Path

import seed_automations
from spec import (
    RemoteMcpServer,
    Spec,
    SpecError,
    load_spec,
    mcp_to_cli_args,
    required_env_for_auth_choice,
)

SPEC_PATH = Path("/opt/agent/spec.json")
SEED_BASE = Path("/opt/seed")

# Do NOT set OPENCLAW_HOME: OpenClaw treats it as a home dir and appends
# .openclaw/ within it (double-nesting). The default (~/.openclaw) is correct.
os.environ.pop("OPENCLAW_HOME", None)


def _timestamp() -> str:
    """OpenClaw log-line timestamp: ISO-8601, millisecond precision, UTC."""
    return datetime.now(UTC).isoformat(timespec="milliseconds")


def log(msg: str) -> None:
    print(f"{_timestamp()} [agent-entry] [info] {msg}", flush=True)


_boot_warnings: list[str] = []


def warn(msg: str) -> None:
    _boot_warnings.append(msg)
    print(f"{_timestamp()} [agent-entry] [warn] {msg}", file=sys.stderr, flush=True)


def run(
    *args: str, check: bool = True, capture: bool = False, timeout: float | None = None
) -> subprocess.CompletedProcess[str] | None:
    """Spawn a CLI command; check=True propagates failure, else the failed
    result (or exception object) is returned for the caller to inspect.
    With timeout set, an expired spawn returns None when check=False (so
    timeout maps to the existing "could not run" contract) and raises
    otherwise."""
    kwargs: dict[str, object] = {}
    if timeout is not None:
        kwargs["timeout"] = timeout
    try:
        return subprocess.run(
            list(args),
            check=check,
            capture_output=capture,
            text=True,
            **kwargs,
        )
    except subprocess.TimeoutExpired:
        if check:
            raise
        return None
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


def config_set_batch(ops: list[tuple[str, object]], *, force: bool = False) -> None:
    """Set several config keys in ONE ``openclaw config set --batch-json``
    call. Ops whose stored value already matches are dropped first (the
    same fast path as config_set — a batch of no-ops shells out zero
    times); a batch that survives the filter costs one openclaw.json
    write and one gateway reload evaluation instead of one per key.
    force skips the no-op filter for keys the caller itself set moments
    ago — their on-disk state is known-changed, so filtering against a
    possibly-stale snapshot is wrong. Values are JSON-typed. Failure
    warns (naming the paths only — values may carry secrets) and never
    raises; the next boot retries."""
    config = read_openclaw_config() if not force else None
    pending: list[dict[str, object]] = []
    for path, value in ops:
        if config is not None:
            current = lookup_config_path(config, path)
            if config_value_matches(current, value, json.dumps(value)):
                continue
        pending.append({"path": path, "value": value})
    if not pending:
        return
    config_reconcile_stats["applied"] += len(pending)
    result = run("openclaw", "config", "set", "--batch-json", json.dumps(pending), check=False)
    if result is not None and result.returncode != 0:
        warn(f"config set batch failed: {', '.join(str(op['path']) for op in pending)}")


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

    _snapshot_base_plugins()

    log("Infrastructure setup complete (MCP + plugins reconciled every boot)")


def _snapshot_base_plugins() -> None:
    """Record the non-bundled plugins present at the end of first boot —
    the setup auth provider and llama-cpp — as {data}/agent-managed-plugins.
    The orphan report never flags these. A missing marker (volumes from
    older images) disables the report rather than mis-attributing."""
    result = run("openclaw", "plugins", "list", "--json", check=False, capture=True)
    if result is None or result.returncode != 0:
        return
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return
    installed = data.get("plugins") if isinstance(data, dict) else None
    if not isinstance(installed, list):
        return
    base_ids = [
        entry.get("id")
        for entry in installed
        if isinstance(entry, dict)
        and isinstance(entry.get("id"), str)
        and entry.get("origin") != "bundled"
    ]
    try:
        marker = data_dir() / "agent-managed-plugins"
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text(json.dumps(base_ids), encoding="utf-8")
    except OSError:
        warn("could not write agent-managed-plugins marker")


# Default tool denials for the agent's own turns: the recursion/spawn
# surfaces (OWASP ASI06 class — a scheduled turn reaching the cron tool
# can self-replicate jobs; spawn chains multiply blast radius).
# heartbeat_respond stays allowed so heartbeat delivery keeps working.
# Any env-active spec config entry under tools.* disables the base
# default (operator owns tool policy from then on; entries whose if_env
# guard never fires configure nothing).
TOOLS_DENY_DEFAULT = ("cron", "subagents", "sessions_spawn", "nodes")


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

    spec_owns_tools = any(
        entry.path.startswith("tools.") and entry.env_guard_satisfied(env)
        for entry in spec.config_entries
    )
    if not spec_owns_tools:
        log("Applying base tools.deny default (agent tool policy unconfigured)")
        config_set("tools.deny", json.dumps(list(TOOLS_DENY_DEFAULT)), "--strict-json")

    _seed_plugins_allow(spec, env)

    if spec.features.gateway_auth:
        if "OPENCLAW_GATEWAY_TOKEN" not in env:
            warn(
                "features.gateway_auth set but OPENCLAW_GATEWAY_TOKEN absent — "
                "gateway auth NOT armed"
            )
        else:
            log("Gateway auth armed by OPENCLAW_GATEWAY_TOKEN (features.gateway_auth)")
            _retire_legacy_gateway_auth_pair(spec, env)


LEGACY_GATEWAY_AUTH_PAIR = (
    (
        "gateway.auth.token",
        {"source": "env", "provider": "default", "id": "OPENCLAW_GATEWAY_TOKEN"},
    ),
    ("secrets.providers.default", {"source": "env"}),
)


def _retire_legacy_gateway_auth_pair(spec: Spec, env: Mapping[str, str]) -> None:
    """Remove the gateway-auth config pair older base images wrote.

    The gateway reads OPENCLAW_GATEWAY_TOKEN natively and that env surface
    WINS (verified in the pinned gateway source: the credential plan
    marks the config surface inactive with "gateway token env var is
    configured"), so a secretRef under gateway.auth.token is by
    construction inactive — the gateway logs SECRETS_GATEWAY_AUTH_SURFACE
    and SECRETS_REF_IGNORED_INACTIVE_SURFACE on every reload evaluation
    while it sits there. Only keys whose stored value exactly matches the
    legacy pair are unset, and only when no env-active spec entry owns
    the path — operator-configured values are never touched. Failures
    warn and never raise."""
    spec_paths = {entry.path for entry in spec.config_entries if entry.env_guard_satisfied(env)}
    config = read_openclaw_config()
    if config is None:
        return
    for path, legacy in sorted(LEGACY_GATEWAY_AUTH_PAIR):
        if path in spec_paths:
            continue
        if lookup_config_path(config, path) != legacy:
            continue
        result = run("openclaw", "config", "unset", path, check=False)
        if result is not None and result.returncode != 0:
            warn(f"config unset failed: {path}")
        else:
            log(f"retired legacy gateway-auth key '{path}' (env var is the active surface)")


PLUGINS_ALLOW_PATH = "plugins.allow"


def _seed_plugins_allow(spec: Spec, env: Mapping[str, str]) -> None:
    """Seed plugins.allow from the base's own plugin footprint so the
    gateway's empty-allowlist warning ("discovered non-bundled plugins
    may auto-load") never fires for a consumer that did not configure
    one. Seeded once: any existing plugins.allow — or an env-active spec
    entry for it — is the operator's and is never clobbered. The seed is
    the first-boot base-plugin snapshot ({data}/agent-managed-plugins)
    plus the spec's own plugin names; no snapshot (pre-marker volume or
    failed first boot) seeds nothing, mirroring the plugin orphan
    report's disable rule. Failures warn via config_set and never
    raise."""
    if any(
        entry.path == PLUGINS_ALLOW_PATH and entry.env_guard_satisfied(env)
        for entry in spec.config_entries
    ):
        return
    config = read_openclaw_config()
    if config is not None and lookup_config_path(config, PLUGINS_ALLOW_PATH) is not _MISSING:
        return
    try:
        snapshot = json.loads((data_dir() / "agent-managed-plugins").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return
    if not isinstance(snapshot, list):
        return
    allow = sorted(
        {name for name in snapshot if isinstance(name, str)}
        | {plugin.name for plugin in spec.plugins}
    )
    if not allow:
        return
    log(f"Seeding plugins.allow with the base default ({', '.join(allow)})")
    config_set(PLUGINS_ALLOW_PATH, json.dumps(allow), "--strict-json")


def _mcp_listing_names() -> set[str] | None:
    """Server names from `openclaw mcp list --json`, or None when the
    listing fails or will not parse. The CLI has emitted two shapes — a
    name-keyed object ({"acme": {...}}) and an enveloped variant
    ({"servers": ["acme"] | [{"name": "acme"}]}) — both parse; names are
    matched structurally, never as substrings of the raw output."""
    result = run("openclaw", "mcp", "list", "--json", check=False, capture=True)
    if result is None or result.returncode != 0:
        return None
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return None

    def names_from(items: object) -> set[str]:
        names: set[str] = set()
        if isinstance(items, list):
            for entry in items:
                if isinstance(entry, str):
                    names.add(entry)
                elif isinstance(entry, dict) and isinstance(entry.get("name"), str):
                    names.add(entry["name"])
        return names

    if isinstance(data, dict):
        servers = data.get("servers")
        if isinstance(servers, list):
            return names_from(servers)
        return set(data)
    return names_from(data)


def mcp_exists(name: str) -> bool:
    """True iff `openclaw mcp list --json` lists the server by name. Any CLI
    failure or unparseable output counts as absent (the subsequent add
    self-heals)."""
    names = _mcp_listing_names()
    return names is not None and name in names


def _read_mcp_args_marker() -> dict[str, str]:
    """Name → args-digest map of the flags the boot last added per server.
    Unreadable or malformed markers read as empty (every existing server
    then converges to the spec once)."""
    try:
        raw = (data_dir() / "agent-mcp-args").read_text(encoding="utf-8")
    except OSError:
        return {}
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError:
        return {}
    if not isinstance(parsed, dict):
        return {}
    return {k: v for k, v in parsed.items() if isinstance(k, str) and isinstance(v, str)}


def _write_mcp_args_marker(digests: Mapping[str, str]) -> None:
    """Persist the args digests; best-effort (warn on failure, never raise)."""
    try:
        path = data_dir() / "agent-mcp-args"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(digests), encoding="utf-8")
    except OSError:
        warn("could not update agent-mcp-args marker")


def _mcp_args_digest(args: list[str]) -> str:
    """Stable digest of the resolved add flags. Only the one-way hash is
    persisted — resolved flags can embed secrets ({env:...} headers) and
    must never hit disk in the marker."""
    return hashlib.sha256("\0".join(args).encode("utf-8")).hexdigest()


def _mcp_add(name: str, args: list[str]) -> bool:
    """Run `openclaw mcp add`; warn on failure (naming the server, never
    values) and report success so callers can gate digest recording."""
    result = run("openclaw", "mcp", "add", name, *args, check=False)
    if result is not None and result.returncode != 0:
        warn(f"mcp add failed: {name}")
        return False
    return True


def reconcile_mcp(spec: Spec, env: Mapping[str, str]) -> None:
    """Register each spec MCP server when missing, and re-register an
    existing one when its resolved flags drift from the last boot's
    (headers, URL, timeout — {env:...} rotation included). Drift detection
    digests the resolved add flags and compares against the
    {data}/agent-mcp-args marker, because `mcp list` carries no per-server
    fields to diff; a hand-registered or pre-marker server therefore
    converges to the spec on its next boot. A failed re-add leaves the
    server absent and the digest stale, so the next boot retries. Flags
    come from mcp_to_cli_args (--no-probe is included by that builder).
    Servers whose if_env guard is unsatisfied are skipped with a log line.
    A warn-only report lists registered servers nothing in the spec accounts
    for. Removal of de-specified managed servers runs only under
    features.mcp_prune (default off — the ownership marker lives in
    agent-writable {data}). Failures warn and never raise."""
    digests = _read_mcp_args_marker()
    original = dict(digests)
    for server in spec.mcp_servers:
        if not guard_satisfied(server.if_env, env):
            log(f"MCP server '{server.name}' skipped (if_env unsatisfied)")
            continue
        args = mcp_to_cli_args(server)
        digest = _mcp_args_digest(args)
        if not mcp_exists(server.name):
            log(f"Registering MCP server '{server.name}'")
            if _mcp_add(server.name, args):
                digests[server.name] = digest
        elif digests.get(server.name) == digest:
            log(f"MCP server '{server.name}' already registered — skipping")
        else:
            why = "spec changed" if server.name in digests else "not yet args-tracked"
            log(f"MCP server '{server.name}' {why} — re-registering")
            result = run("openclaw", "mcp", "unset", server.name, check=False)
            if result is not None and result.returncode != 0:
                warn(f"mcp unset failed: {server.name}")
            if _mcp_add(server.name, args):
                digests[server.name] = digest
        for key, value in server.passthrough_config.items():
            config_set(f"mcp.servers.{server.name}.{key}", json.dumps(value), "--strict-json")
        if isinstance(server, RemoteMcpServer) and server.auth == "oauth":
            config_set(f"mcp.servers.{server.name}.auth", json.dumps(server.auth), "--strict-json")
            if server.oauth:
                config_set(
                    f"mcp.servers.{server.name}.oauth", json.dumps(server.oauth), "--strict-json"
                )

    if spec.features.mcp_prune:
        _reconcile_managed_mcp(spec)
    _report_orphan_mcp(spec)
    if digests != original:
        _write_mcp_args_marker(digests)


def _reconcile_managed_mcp(spec: Spec) -> None:
    """Ownership-marked removal: only servers recorded in the marker — ones
    a previous base boot registered — and absent from the CURRENT spec
    (including if_env-skipped entries, which are still spec'd) are unset.
    The marker converges to the spec's server list plus failed removals."""
    marker = data_dir() / "agent-managed-mcp"
    try:
        managed_raw = json.loads(marker.read_text(encoding="utf-8"))
        managed = (
            [name for name in managed_raw if isinstance(name, str)]
            if isinstance(managed_raw, list)
            else []
        )
    except (OSError, json.JSONDecodeError):
        managed = []

    spec_names = [server.name for server in spec.mcp_servers]
    still_managed = [name for name in managed if name in spec_names]
    for name in managed:
        if name in spec_names:
            continue
        if not mcp_exists(name):
            continue
        result = run("openclaw", "mcp", "unset", name, check=False)
        if result is not None and result.returncode == 0:
            log(f"Removed MCP server '{name}' (no longer in spec)")
            continue
        warn(f"mcp unset failed: {name} (will retry next boot)")
        still_managed.append(name)

    converged = spec_names + [name for name in still_managed if name not in spec_names]
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text(json.dumps(converged), encoding="utf-8")
    except OSError:
        warn("could not update agent-managed-mcp marker")


def _report_orphan_mcp(spec: Spec) -> None:
    """Warn-only diff of registered MCP servers against the spec surface.
    The reconcile never prunes config it does not own, and the ownership
    marker lives in agent-writable {data} — so a server registered by hand
    or by a compromised agent is indistinguishable from an operator edit
    unless the boot says so out loud. Removal stays the operator's call
    (features.mcp_prune for base-managed entries)."""
    names = _mcp_listing_names()
    if names is None:
        return
    spec_names = {server.name for server in spec.mcp_servers}
    for name in sorted(names - spec_names):
        warn(f"MCP server '{name}' registered but not in spec")


def _plugin_listing_ids() -> set[str] | None:
    """Plugin ids from `openclaw plugins list --json`, or None when the
    listing fails or will not parse. Both emitted shapes parse: enveloped
    ({"plugins": [{"id": ...}]}) and name-keyed ({"acme": {}})."""
    result = run("openclaw", "plugins", "list", "--json", check=False, capture=True)
    if result is None or result.returncode != 0:
        return None
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return None

    def ids_from(items: object) -> set[str]:
        ids: set[str] = set()
        if isinstance(items, list):
            for entry in items:
                if isinstance(entry, dict) and isinstance(entry.get("id"), str):
                    ids.add(entry["id"])
        return ids

    if isinstance(data, dict):
        plugins = data.get("plugins")
        if isinstance(plugins, list):
            return ids_from(plugins)
        return set(data)
    return ids_from(data)


def plugin_exists(name: str) -> bool:
    """True iff `openclaw plugins list --json` carries the plugin id. Any
    CLI failure or unparseable output counts as absent."""
    ids = _plugin_listing_ids()
    return ids is not None and name in ids


def reconcile_plugins(spec: Spec) -> None:
    """Local-source plugins (absolute path) are force-installed on every
    boot — their content is image-baked and must track the image. Registry
    plugins install only when absent. Non-bundled registry plugins absent
    from the spec surface as a warn-only orphan report. Failures warn and
    never raise."""
    for plugin in spec.plugins:
        if plugin.source is not None:
            log(f"Reconciling local plugin '{plugin.name}'")
            result = run("openclaw", "plugins", "install", plugin.source, "--force", check=False)
            if result is not None and result.returncode != 0:
                warn(f"plugin install failed: {plugin.name}")
        elif not plugin_exists(plugin.name):
            log(f"Installing plugin '{plugin.name}'")
            result = run("openclaw", "plugins", "install", plugin.name, check=False)
            if result is not None and result.returncode != 0:
                warn(f"plugin install failed: {plugin.name}")
        else:
            log(f"Plugin '{plugin.name}' already installed — skipping")

    _report_orphan_plugins(spec)
    if spec.features.plugin_prune:
        _prune_despecified_plugins(spec)


def _installed_plugin_ids() -> set[str] | None:
    """Non-bundled plugin ids from `plugins list --json`, or None when the
    listing is unavailable/unparseable."""
    result = run("openclaw", "plugins", "list", "--json", check=False, capture=True)
    if result is None or result.returncode != 0:
        return None
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return None
    installed = data.get("plugins") if isinstance(data, dict) else None
    if not isinstance(installed, list):
        return None
    ids: set[str] = set()
    for entry in installed:
        if not isinstance(entry, dict):
            continue
        plugin_id = entry.get("id")
        if isinstance(plugin_id, str) and entry.get("origin") != "bundled":
            ids.add(plugin_id)
    return ids


def _prune_despecified_plugins(spec: Spec) -> None:
    """features.plugin_prune: uninstall plugins the base installed from an
    earlier spec that the current spec dropped (ownership:
    {data}/agent-managed-spec-plugins). Operator installs are never
    touched; failed uninstalls warn and retry next boot. The marker
    converges to the spec's plugin list on every reconcile — pruning
    ownership accrues while the feature is enabled."""
    marker = data_dir() / "agent-managed-spec-plugins"
    try:
        managed_raw = json.loads(marker.read_text(encoding="utf-8"))
        managed = (
            [n for n in managed_raw if isinstance(n, str)] if isinstance(managed_raw, list) else []
        )
    except (OSError, json.JSONDecodeError):
        managed = []

    spec_names = [plugin.name for plugin in spec.plugins]
    installed = _installed_plugin_ids()
    still_managed = [name for name in managed if name in spec_names]
    if installed is not None:
        for name in managed:
            if name in spec_names or name not in installed:
                continue
            result = run("openclaw", "plugins", "uninstall", name, check=False)
            if result is not None and result.returncode == 0:
                log(f"Removed plugin '{name}' (no longer in spec)")
                continue
            warn(f"plugin uninstall failed: {name} (will retry next boot)")
            still_managed.append(name)

    converged = spec_names + [name for name in still_managed if name not in spec_names]
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text(json.dumps(converged), encoding="utf-8")
    except OSError:
        warn("could not update agent-managed-spec-plugins marker")


def _report_orphan_plugins(spec: Spec) -> None:
    """Warn-once-per-boot diff of installed vs spec'd registry plugins.
    The base's own installs (marker from first boot) and bundled plugins
    are never orphans. No marker (older-image volume) disables the report;
    removal stays the operator's call — plugin installs carry no
    per-install ownership record the MCP marker can rely on."""
    try:
        base_raw = json.loads((data_dir() / "agent-managed-plugins").read_text(encoding="utf-8"))
        base_ids = (
            {name for name in base_raw if isinstance(name, str)}
            if isinstance(base_raw, list)
            else set()
        )
    except (OSError, json.JSONDecodeError):
        return

    result = run("openclaw", "plugins", "list", "--json", check=False, capture=True)
    if result is None or result.returncode != 0:
        return
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return
    installed = data.get("plugins") if isinstance(data, dict) else None
    if not isinstance(installed, list):
        return

    spec_names = {plugin.name for plugin in spec.plugins}
    for entry in installed:
        if not isinstance(entry, dict):
            continue
        plugin_id = entry.get("id")
        if not isinstance(plugin_id, str):
            continue
        if entry.get("origin") == "bundled" or plugin_id in spec_names or plugin_id in base_ids:
            continue
        warn(f"plugin '{plugin_id}' installed but not in spec")


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


def _seed_dir_safe(path: Path) -> bool:
    """False when path is a symlink or any non-directory node (FIFO, file).
    Seeded roots are replaced wholesale and written into, and {data} is
    agent-writable — an unexpected node type must refuse rather than
    rmtree/copytree/mkdir through it (an uncaught OSError here crash-loops
    the boot; a followed symlink redirects writes and deletes)."""
    if path.is_symlink():
        return False
    return not path.exists() or path.is_dir()


def seed_content(spec: Spec, env: Mapping[str, str]) -> None:
    """Seed content from SEED_BASE subdirs into {data}.

    workspace/ is copied on first boot only (the agent evolves it at
    runtime); skills/ and docs/ are fully replaced every boot (image-baked
    reference content; docs land at {data}/workspace/docs). journal/ is
    created with parents. Seeded roots that are symlinks or non-directory
    nodes are refused with a warning (never followed, never deleted
    through) — the boot continues with that root unseeded. AGENT_SKIP_SEED=1
    skips content seeding only — config/mcp/plugin reconciliation is the
    caller's business and still runs."""
    if env.get("AGENT_SKIP_SEED", "0") == "1":
        log("Content seeding skipped (AGENT_SKIP_SEED=1 — dev mode bind mounts)")
        return

    workspace = data_dir() / "workspace"
    workspace_src = SEED_BASE / "workspace"
    if not _seed_dir_safe(workspace):
        warn(f"refusing to seed: {workspace} is a symlink or not a directory")
        return
    if workspace_src.is_dir():
        if not workspace.exists():
            shutil.copytree(workspace_src, workspace)
        elif env.get("AGENT_SYNC", "0") == "1":
            log("AGENT_SYNC=1 — re-seeding workspace (seeded files overwritten)")
            shutil.copytree(workspace_src, workspace, dirs_exist_ok=True)
        else:
            workspace.mkdir(parents=True, exist_ok=True)
    else:
        workspace.mkdir(parents=True, exist_ok=True)

    skills_src = SEED_BASE / "skills"
    if skills_src.is_dir():
        skills_dst = data_dir() / "skills"
        if not _seed_dir_safe(skills_dst):
            warn(f"refusing to replace skills: {skills_dst} is a symlink or not a directory")
        else:
            if skills_dst.exists():
                shutil.rmtree(skills_dst)
            shutil.copytree(skills_src, skills_dst)

    docs_src = SEED_BASE / "docs"
    if docs_src.is_dir():
        docs_dst = workspace / "docs"
        if not _seed_dir_safe(docs_dst):
            warn(f"refusing to replace docs: {docs_dst} is a symlink or not a directory")
        else:
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
    cmd = ["openclaw", "memory", "index"]
    if force:
        cmd.append("--force")
    cmd += ["--agent", agent, "--verbose"]
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


def _parse_doctor_stdout(result: subprocess.CompletedProcess[str]) -> dict | None:
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return None
    return data if isinstance(data, dict) else None


DOCTOR_SKILLS_CHECK = "core/doctor/skills-readiness"
DOCTOR_DISABLED_MARKER = "doctor-disabled-skills"
DOCTOR_HEAL_ATTEMPTS_MARKER = "doctor-heal-attempts"
DOCTOR_SETTLE_S = 30


def _skills_readiness_flagged(data: dict) -> set[str]:
    """Skill names flagged by the skills-readiness check in parsed doctor
    output (findings carry path ``skills.entries.<name>.enabled``)."""
    prefix = "skills.entries."
    suffix = ".enabled"
    flagged: set[str] = set()
    for finding in data.get("findings", []):
        if finding.get("checkId") != DOCTOR_SKILLS_CHECK:
            continue
        path = finding.get("path", "")
        if path.startswith(prefix) and path.endswith(suffix):
            name = path[len(prefix) : -len(suffix)]
            if name:
                flagged.add(name)
    return flagged


def _confirm_skills_flagged() -> set[str] | None:
    """Second, settle-spaced doctor run scoped to the skills check
    (``doctor --lint --only core/doctor/skills-readiness``). Returns the
    flagged set, or None when the run fails or will not parse — the
    caller then falls back to the first verdict."""
    result = run(
        "openclaw", "doctor", "--lint", "--only", DOCTOR_SKILLS_CHECK, check=False, capture=True
    )
    if result is None:
        return None
    data = _parse_doctor_stdout(result)
    return None if data is None else _skills_readiness_flagged(data)


def _read_heal_attempts() -> dict[str, str]:
    """Skill name → image version of its last failed heal attempt, from
    {data}/doctor-heal-attempts. Unreadable/malformed markers read as
    empty (heal retries are simply no longer deferred)."""
    try:
        parsed = json.loads((data_dir() / DOCTOR_HEAL_ATTEMPTS_MARKER).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}
    if not isinstance(parsed, dict):
        return {}
    return {k: v for k, v in parsed.items() if isinstance(k, str) and isinstance(v, str)}


def _write_heal_attempts(attempts: Mapping[str, str]) -> None:
    """Persist the heal-attempts map; best-effort (warn on failure)."""
    try:
        path = data_dir() / DOCTOR_HEAL_ATTEMPTS_MARKER
        if attempts:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(dict(sorted(attempts.items()))), encoding="utf-8")
        else:
            path.unlink(missing_ok=True)
    except OSError:
        warn("could not update doctor-heal-attempts marker")


def disable_unavailable_skills(data: dict | None = None) -> None:
    """Reconcile skill enablement against `openclaw doctor --lint`,
    stably.

    The check only examines ENABLED skills ("allowed skills are usable"),
    so a skill this reconcile disabled is never flagged on the next boot —
    a cleared finding on a disabled skill is NOT proof of recovery. The
    old one-shot verdict therefore oscillated: disable → finding cleared
    (because disabled) → re-enable → flagged again, one write per skill
    per boot.

    Stability rules:
    - A disable requires the finding to persist across TWO doctor runs
      (the second is settle-spaced to ride out plugin/MCP pre-warm and
      skill-snapshot reload); transient findings settle clear and are
      never written.
    - A heal is PROVEN, not assumed: candidates are re-enabled first so
      the confirm run can actually observe them, then any that re-flag
      are disabled again in the same boot. Failed heals defer their retry
      until AGENT_BASE_VERSION changes ({data}/doctor-heal-attempts) —
      requirements arrive with image bumps; delete the marker to force a
      retry.
    - Every phase writes through one `config set --batch-json` call, so a
      transition boot costs at most two CLI writes and a steady-state
      boot (unchanged env, converged verdicts) writes nothing at all.

    Operator intent is never overridden: enabled=false without the marker
    (spec config entry, hand-managed openclaw.json) stays off. `doctor
    --lint` exits 1 iff any finding exists, so stdout is authoritative
    and the exit code is ignored. Never raises. Pass the parsed doctor
    output to share a single doctor run with the post-startup
    diagnostics; the skills reconcile itself runs doctor only when not
    given one."""
    if data is None:
        result = run("openclaw", "doctor", "--lint", check=False, capture=True)
        if result is None:
            return
        data = _parse_doctor_stdout(result)
        if data is None:
            return

    flagged_first = _skills_readiness_flagged(data)
    marker = data_dir() / DOCTOR_DISABLED_MARKER
    try:
        previously_disabled = {
            line for line in marker.read_text(encoding="utf-8").splitlines() if line
        }
    except OSError:
        previously_disabled = set()

    # Absent openclaw.json means skills come back enabled-by-default —
    # stale marker entries drop. Only a present-but-corrupt file (read
    # fails) conserves the marker.
    config = read_openclaw_config()
    can_verify = config is not None or not (data_dir() / "openclaw.json").exists()

    fresh_disables = flagged_first - previously_disabled
    heal_candidates: set[str] = set()
    stale: set[str] = set()
    attempts = _read_heal_attempts()
    image_version = os.environ.get("AGENT_BASE_VERSION", "")
    deferred_heals: set[str] = set()
    for name in sorted(previously_disabled - flagged_first):
        entry = lookup_config_path(config, f"skills.entries.{name}") if config else _MISSING
        if isinstance(entry, dict) and entry.get("enabled") is False:
            if attempts.get(name) == image_version:
                deferred_heals.add(name)
            else:
                heal_candidates.add(name)
        elif can_verify:
            stale.add(name)

    healed: set[str] = set()
    confirmed_disables: set[str] = set()
    reflagged: set[str] = set()

    if fresh_disables or heal_candidates:
        if heal_candidates:
            for name in sorted(heal_candidates):
                log(f"re-enabling skill '{name}' for doctor confirmation")
            config_set_batch(
                [(f"skills.entries.{name}.enabled", True) for name in sorted(heal_candidates)]
            )
        time.sleep(DOCTOR_SETTLE_S)
        confirmed = _confirm_skills_flagged()
        if confirmed is None:
            warn("doctor confirm run failed — applying first-run verdicts")
            confirmed = flagged_first

        reflagged = heal_candidates & confirmed
        healed = heal_candidates - confirmed - reflagged
        confirmed_disables = fresh_disables & confirmed
        transient = fresh_disables - confirmed

        disable_ops = sorted(reflagged | confirmed_disables)
        if disable_ops:
            for name in sorted(confirmed_disables):
                log(f"disabling unavailable skill '{name}' (doctor skills-readiness)")
            for name in sorted(reflagged):
                log(f"re-disabling skill '{name}' (still unavailable after re-enable)")
            config_set_batch(
                [(f"skills.entries.{name}.enabled", False) for name in sorted(confirmed_disables)]
            )
            config_set_batch(
                [(f"skills.entries.{name}.enabled", False) for name in sorted(reflagged)],
                force=True,
            )
        for name in sorted(healed):
            log(f"skill '{name}' re-enable confirmed (skills-readiness finding cleared)")
        for name in sorted(transient):
            log(f"skill '{name}' doctor finding settled clear — leaving enabled")

    if deferred_heals:
        log(
            f"deferring {len(deferred_heals)} heal re-check(s) until the image version "
            f"changes (remove {{data}}/{DOCTOR_HEAL_ATTEMPTS_MARKER} to force)"
        )

    # Failed/unconfirmed heals retry only on an image change; every name
    # this boot disabled records its version so the next boot defers
    # instead of re-entering the enable→re-flag→disable cycle.
    remaining_names = (previously_disabled - healed - stale) | confirmed_disables | reflagged
    attempts_out = {
        name: image_version
        for name, version in attempts.items()
        if name in remaining_names and name not in healed
    }
    for name in sorted(reflagged | confirmed_disables):
        attempts_out[name] = image_version
    if attempts_out != attempts:
        _write_heal_attempts(attempts_out)

    remaining = sorted(remaining_names)
    try:
        if remaining:
            marker.write_text("\n".join(remaining) + "\n", encoding="utf-8")
        else:
            marker.unlink(missing_ok=True)
    except OSError:
        warn("could not update doctor-disabled-skills marker")


def run_seed_automations(model: str, default_tools: tuple[str, ...] = ()) -> int:
    """Invoke seed_automations.main in-process with the spec's automation
    model (and, when the spec sets one, its default tool allow-list). The
    automations dir (AGENT_AUTOMATIONS_DIR, default /opt/agent/automations)
    is resolved inside that module. Returns the would-be exit code (main()
    signals failure via SystemExit)."""
    argv = ["--model", model]
    if default_tools:
        argv.extend(("--default-tools", ",".join(default_tools)))
    try:
        seed_automations.main(argv)
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
    cron_exit = run_seed_automations(spec.automations_model, spec.automations_default_tools)
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

    doctor_result = run("openclaw", "doctor", "--lint", check=False, capture=True)
    doctor_data = _parse_doctor_stdout(doctor_result) if doctor_result is not None else None
    disable_unavailable_skills(doctor_data)
    _surface_doctor(doctor_result, doctor_data)

    result = run("openclaw", "config", "validate", check=False, capture=True)
    if result is not None and result.returncode != 0:
        warn("config validation found issues")

    _run_security_audit()
    _write_boot_status()


def _log_finding_details(kind: str, findings: list[object], limit: int = 10) -> None:
    """Bounded per-finding detail after a count line: checkId (or id) plus
    path — never the message text, which may quote secret values. First
    `limit` findings only; the full report is already persisted under
    {data}/logs for the rest."""
    shown = 0
    for finding in findings:
        if shown >= limit:
            break
        if not isinstance(finding, dict):
            continue
        check = finding.get("checkId", finding.get("id"))
        if not isinstance(check, str):
            check = "?"
        path = finding.get("path")
        suffix = f" {path}" if isinstance(path, str) and path else ""
        log(f"{kind} finding: {check}{suffix}")
        shown += 1
    if len(findings) > limit:
        log(f"{kind}: {len(findings) - limit} more finding(s) in the persisted report")


def _surface_doctor(result: subprocess.CompletedProcess[str] | None, data: dict | None) -> None:
    """Log the doctor summary and persist the full report when findings
    exist ({data}/logs/doctor-report.json). Clean boots leave no file."""
    if result is not None and result.returncode != 0:
        warn("doctor lint found issues")
    if data is None:
        return
    findings = data.get("findings")
    if not isinstance(findings, list) or not findings:
        return
    log(f"doctor: {len(findings)} finding(s), {data.get('checksRun', '?')} checks run")
    _log_finding_details("doctor", findings)
    _persist_report("doctor-report.json", data)


def _run_security_audit() -> None:
    """Warn-only `openclaw security audit` — a cheap post-boot check for
    common foot-guns. Findings are logged and persisted
    ({data}/logs/security-report.json); never gates anything."""
    result = run("openclaw", "security", "audit", "--json", check=False, capture=True, timeout=120)
    if result is None or result.returncode != 0:
        return
    try:
        data = json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        return
    if isinstance(data, list):
        findings = data
    elif isinstance(data, dict) and isinstance(data.get("findings"), list):
        findings = data["findings"]
    else:
        return
    if findings:
        log(f"security audit: {len(findings)} finding(s)")
        _log_finding_details("security", findings)
        _persist_report("security-report.json", {"findings": findings})


def _persist_report(name: str, data: dict) -> None:
    try:
        reports = data_dir() / "logs"
        reports.mkdir(parents=True, exist_ok=True)
        (reports / name).write_text(json.dumps(data), encoding="utf-8")
    except OSError:
        warn(f"could not persist {name}")


def _write_boot_status() -> None:
    """Best-effort boot summary at {data}/status.json: image version and
    the boot's warning count (a number only — warning text never touches
    disk). Written at the end of a completed post-startup."""
    try:
        status = {
            "imageVersion": os.environ.get("AGENT_BASE_VERSION", ""),
            "warnings": len(_boot_warnings),
            "bootCompletedAt": datetime.now(UTC).isoformat(),
        }
        data_dir().mkdir(parents=True, exist_ok=True)
        (data_dir() / "status.json").write_text(json.dumps(status), encoding="utf-8")
    except OSError:
        pass


def backup_before_upgrade(env: Mapping[str, str]) -> None:
    """Verified backup before an image-version delta touches a warm volume.

    The image version comes from AGENT_BASE_VERSION (Dockerfile ENV, baked
    from the build ARG); the last-seen version lives in
    {data}/last-image-version. A warm volume (openclaw.json present) whose
    marker differs — including volumes from pre-marker images — gets
    `openclaw backup create --verify --output /backups` (AGENT_BACKUP_DIR
    overrides) BEFORE any other phase mutates state. Failure aborts the boot
    (exit 1): data
    safety outranks gateway availability for a migration event, and the
    un-updated marker makes the next boot retry. Fresh volumes record the
    version without a backup. Dev boots without the env var skip entirely."""
    version = env.get("AGENT_BASE_VERSION", "").strip()
    if not version:
        log("image version unknown (dev boot) — skipping upgrade backup check")
        return

    marker = data_dir() / "last-image-version"
    try:
        previous = marker.read_text(encoding="utf-8").strip()
    except OSError:
        previous = ""
    if previous == version:
        return

    # The CLI refuses output inside its source tree ({data}), so backups
    # default to the dedicated /backups path — mount a named volume there.
    if (data_dir() / "openclaw.json").exists():
        backups = Path(env.get("AGENT_BACKUP_DIR", "/backups"))
        log(f"Image changed ({previous or 'unknown'} → {version}) — creating verified backup")
        try:
            backups.mkdir(parents=True, exist_ok=True)
        except OSError:
            warn("upgrade backup failed: cannot create backups directory — aborting boot")
            raise SystemExit(1) from None
        result = run(
            "openclaw",
            "backup",
            "create",
            "--verify",
            "--output",
            str(backups),
            check=False,
            timeout=600,
        )
        if result is None or result.returncode != 0:
            warn("upgrade backup failed — aborting boot (fix the backup, then restart)")
            raise SystemExit(1)

    try:
        marker.write_text(version, encoding="utf-8")
    except OSError:
        warn("could not record image version (backup check will rerun next boot)")


# --- shutdown supervision (graceful stop: drain in-flight automations) ---

_SHUTDOWN_SIGNALS = (signal.SIGTERM, signal.SIGINT)
_SHUTDOWN_POLL_S = 0.2
DEFAULT_SHUTDOWN_GRACE = 600


def parse_shutdown_grace(env: Mapping[str, str]) -> int:
    """AGENT_SHUTDOWN_GRACE in seconds: how long a shutdown signal waits for
    the gateway and its in-flight automations before their process group is
    force-killed. Default 600; 0 forwards the signal then force-kills at
    once; invalid values warn naming the env var and fall back to 600."""
    raw = env.get("AGENT_SHUTDOWN_GRACE", "")
    if not raw:
        return DEFAULT_SHUTDOWN_GRACE
    try:
        grace = int(raw)
    except ValueError:
        warn(f"AGENT_SHUTDOWN_GRACE={raw!r} is not an integer — using {DEFAULT_SHUTDOWN_GRACE}s")
        return DEFAULT_SHUTDOWN_GRACE
    if grace < 0:
        warn(f"AGENT_SHUTDOWN_GRACE={raw!r} is negative — using {DEFAULT_SHUTDOWN_GRACE}s")
        return DEFAULT_SHUTDOWN_GRACE
    return grace


class ShutdownSupervisor:
    """Runs the container CMD as a child in its own session and returns its
    exit code, keeping in-flight automations alive across shutdown signals:

    - the first SIGTERM/SIGINT is forwarded to the CMD pid ONLY — never its
      process group, because the automations in that group must outlive the
      gateway's own shutdown
    - once the CMD has exited, the drain waits for its process group (the
      automation processes; orphaned ones are re-parented to tini, so the
      group is probed with killpg(pgid, 0), not waitpid) to empty, bounded
      by the grace timeout, then SIGKILLs what remains
    - a second signal force-kills the group immediately (operator escape)
    - an unprompted CMD exit also kills the group: identical teardown
      semantics to the exec era, so restart policies fire promptly
    """

    def __init__(
        self,
        *,
        popen: Callable[..., subprocess.Popen] = subprocess.Popen,
        kill: Callable[[int, int], None] = os.kill,
        killpg: Callable[[int, int], None] = os.killpg,
        waitpid: Callable[[int, int], tuple[int, int]] = os.waitpid,
        monotonic: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self._popen = popen
        self._kill = kill
        self._killpg = killpg
        self._waitpid = waitpid
        self._monotonic = monotonic
        self._sleep = sleep
        self._signals: list[int] = []

    def _handle_signal(self, signum: int, frame: object) -> None:
        self._signals.append(signum)

    def supervise(self, command: list[str], grace: int, forward_pids: tuple[int, ...] = ()) -> int:
        saved = [(sig, signal.getsignal(sig)) for sig in _SHUTDOWN_SIGNALS]
        try:
            for sig, _handler in saved:
                signal.signal(sig, self._handle_signal)
            try:
                proc = self._popen(command, start_new_session=True)
            except OSError as exc:
                warn(f"failed to start {command[0]}: {exc}")
                return 1
            log(f"supervising {command[0]} (pid {proc.pid}, shutdown grace {grace}s)")
            return self._run(
                proc, pgid=proc.pid, command=command, grace=grace, forward_pids=forward_pids
            )
        finally:
            for sig, handler in saved:
                signal.signal(sig, handler)

    def _run(
        self,
        proc: subprocess.Popen,
        pgid: int,
        command: list[str],
        grace: int,
        forward_pids: tuple[int, ...],
    ) -> int:
        gateway_status: int | None = None
        forwarded = False
        forced = False
        deadline = 0.0
        while True:
            while True:
                try:
                    pid, status = self._waitpid(-1, os.WNOHANG)
                except ChildProcessError:
                    break
                if pid == 0:
                    break
                if pid == proc.pid:
                    gateway_status = status
                    proc.returncode = os.waitstatus_to_exitcode(status)
            now = self._monotonic()

            if len(self._signals) >= 2 and not forced:
                log("second shutdown signal — force-killing the gateway process group")
                forced = True
                self._killpg_guarded(pgid, signal.SIGKILL)

            if self._signals and not forwarded:
                signum = self._signals[0]
                log(
                    f"shutdown signal {signum}: forwarded to {command[0]}"
                    f" (pid {proc.pid}); draining in-flight automations"
                    f" for up to {grace}s"
                )
                if gateway_status is None:
                    self._kill_guarded(proc.pid, signum)
                for fpid in forward_pids:
                    self._kill_guarded(fpid, signum)
                forwarded = True
                deadline = now + grace

            if gateway_status is None:
                if forwarded and not forced and (grace <= 0 or now >= deadline):
                    log(f"shutdown grace ({grace}s) expired — force-killing")
                    forced = True
                    self._killpg_guarded(pgid, signal.SIGKILL)
                self._sleep(_SHUTDOWN_POLL_S)
                continue

            if not self._signals and not forced:
                log(f"{command[0]} exited unprompted — killing its process group")
                forced = True
                self._killpg_guarded(pgid, signal.SIGKILL)

            if self._signals and not forced:
                if now >= deadline:
                    log(f"shutdown grace ({grace}s) expired — force-killing")
                    forced = True
                    self._killpg_guarded(pgid, signal.SIGKILL)
                elif not self._group_alive(pgid):
                    log("drain complete — gateway process group is empty")
                else:
                    self._sleep(_SHUTDOWN_POLL_S)
                    continue

            code = os.waitstatus_to_exitcode(gateway_status)
            return code if code >= 0 else 128 - code

    def _group_alive(self, pgid: int) -> bool:
        try:
            self._killpg(pgid, 0)
        except ProcessLookupError:
            return False
        except PermissionError:
            return True
        return True

    def _kill_guarded(self, pid: int, signum: int) -> None:
        with suppress(ProcessLookupError):
            self._kill(pid, signum)

    def _killpg_guarded(self, pgid: int, signum: int) -> None:
        with suppress(ProcessLookupError):
            self._killpg(pgid, signum)


def supervise(command: list[str], grace: int, forward_pids: tuple[int, ...] = ()) -> int:
    """Phase function wrapping ShutdownSupervisor (the seam wrapper
    entrypoints and tests patch): spawn the CMD in its own session and
    return its exit code after graceful-shutdown drain."""
    return ShutdownSupervisor().supervise(command, grace, forward_pids)


def validate_spec(env: Mapping[str, str]) -> int:
    """--validate-spec mode for downstream CI: load the spec and the
    automations directory WITHOUT any mutation. Returns 0 when both parse,
    1 (reason on stderr) otherwise."""
    try:
        load_agent_spec(env)
        seed_automations.build_jobs()
    except (SpecError, seed_automations.AutomationSpecError, OSError) as exc:
        print(
            f"{_timestamp()} [agent-entry] [error] spec validation failed: {exc}",
            file=sys.stderr,
            flush=True,
        )
        return 1
    log("spec validation passed")
    return 0


def main(argv: list[str] | None = None) -> int:
    """Boot the agent container, then supervise the container CMD.

    The happy path spawns the CMD via supervise() and returns its exit
    code after graceful-shutdown drain (bounded by AGENT_SHUTDOWN_GRACE).
    Other int returns: --validate-spec (0/1) and usage errors (2)."""
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

    backup_before_upgrade(env)

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

    return supervise(command, parse_shutdown_grace(env), forward_pids=(pid,))


if __name__ == "__main__":
    raise SystemExit(main())
