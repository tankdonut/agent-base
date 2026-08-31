#!/usr/bin/env python3
"""Strict, stdlib-only loader for the standard-agent spec.json (schema v1).

Per-agent boot differences for the shared OpenClaw base image are declared in
a spec.json baked into each project image. This module is the single parser;
the entrypoint consumes the frozen dataclasses returned here and never
re-derives CLI marshalling.

Invariants enforced for every spec:

- Fail-closed: any unknown key at any level aborts the load. There are no
  warnings and no best-effort parsing.
- Every SpecError message starts with the JSON path of the offending node
  (e.g. ``mcp_servers[2].url: ...``). File-level failures (unparseable JSON)
  are prefixed with the file path instead.
- String templating is resolved eagerly at load time against the ``env``
  mapping passed to load_spec; os.environ is never read here. The only
  tokens are ``{env:NAME}`` (missing NAME is an error naming it) and
  ``{data}`` (resolves to ``~/.openclaw``). Substitution is single-pass:
  replacement text is never re-scanned for tokens, and any other brace
  content (or an unclosed brace) is an error.

Run tests: python3 -m unittest discover -s container -p "test_spec.py" -v
"""

# allow: SIZE_OK — the task contract mandates a single-file loader (exactly
# three files: spec.py, test_spec.py, spec.example.json); splitting would
# create forbidden fourth modules.

from __future__ import annotations

import json
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import NoReturn, TypeAlias, assert_never

__all__ = [
    "SPEC_VERSION_SUPPORTED",
    "SpecError",
    "JSONValue",
    "ConfigEntry",
    "Channel",
    "LocalMcpServer",
    "RemoteMcpServer",
    "McpServer",
    "Plugin",
    "Features",
    "Spec",
    "load_spec",
    "mcp_to_cli_args",
]

SPEC_VERSION_SUPPORTED = 1
"""The only specVersion this loader understands; anything else is rejected."""

JSONValue: TypeAlias = str | int | float | bool | None | list["JSONValue"] | dict[str, "JSONValue"]


class SpecError(ValueError):
    """Raised for every spec validation failure (unknown key, bad type,
    missing required section/field, unresolved template token, unsupported
    specVersion, non-absolute plugin source, ...).

    ``str(err)`` always starts with the JSON path of the offending node.
    """


@dataclass(frozen=True, slots=True)
class ConfigEntry:
    """One ``openclaw config set`` directive, pre-marshalled for the CLI.

    Invariants:
    - resolved_value is raw_value with template tokens substituted in every
      string at any nesting depth; raw_value is preserved verbatim.
    - cli_value / use_strict_json are derived and final: the entrypoint hands
      cli_value over as the value argument and appends --strict-json iff
      use_strict_json (strict=true always means JSON, as does any non-str
      value; a plain non-strict str is passed through unquoted).
    - split_csv=true requires the resolved value to be a string; it is split
      on commas (items stripped, empties dropped) and cli_value becomes the
      strict-JSON list of items. An empty result is a SpecError naming the
      referenced env var(s) — a configured CSV that yields nothing is a
      misconfiguration.
    """

    path: str
    raw_value: JSONValue
    resolved_value: JSONValue
    cli_value: str
    use_strict_json: bool
    if_env: tuple[str, ...] = ()
    split_csv: bool = False

    def env_guard_satisfied(self, env: Mapping[str, str]) -> bool:
        """True iff every name in if_env is present in env; empty guard always passes."""
        return all(name in env for name in self.if_env)


@dataclass(frozen=True, slots=True)
class Channel:
    """A channel to enable.

    use_env=True (default): the entrypoint feeds the channel its credentials
    from the environment; False: authentication is left to the runtime.
    """

    type: str
    use_env: bool = True


@dataclass(frozen=True, slots=True)
class LocalMcpServer:
    """A stdio MCP server (``openclaw mcp add NAME --command ...``).

    Invariants: command/args/env values are template-resolved at load time;
    env preserves the spec's insertion order (CLI --env flags are emitted in
    that order) and is never mutated after construction.
    """

    name: str
    command: str
    args: tuple[str, ...] = ()
    env: dict[str, str] = field(default_factory=dict)
    no_probe: bool = True
    timeout: int | None = None
    if_env: tuple[str, ...] = ()
    passthrough_config: dict[str, JSONValue] = field(default_factory=dict)


@dataclass(frozen=True, slots=True)
class RemoteMcpServer:
    """An HTTP MCP server (``openclaw mcp add NAME --url ...``).

    Invariants: url and header values are template-resolved at load time
    ({env:...} for API keys is the intended use); headers preserve insertion
    order; timeout is seconds (CLI contract, verified at 2026.7.1-2).

    auth="oauth" arms OpenClaw's MCP OAuth flow (credentials live in
    OpenClaw's own store after a one-time ``mcp login`` — never in the
    spec); oauth carries the documented metadata sub-keys verbatim,
    never templated (they are structural, not secrets).

    transport pins the remote HTTP transport: "sse" or "streamable-http"
    (the pinned CLI's exact --transport values, verified at 2026.7.1-2).
    None keeps the CLI default — SSE — which POST-only streamable-HTTP
    endpoints reject (405 on the SSE GET handshake), so spec them
    explicitly.
    """

    name: str
    url: str
    headers: dict[str, str] = field(default_factory=dict)
    no_probe: bool = True
    timeout: int | None = None
    transport: str | None = None
    if_env: tuple[str, ...] = ()
    passthrough_config: dict[str, JSONValue] = field(default_factory=dict)
    auth: str | None = None
    oauth: dict[str, str] = field(default_factory=dict)


McpServer: TypeAlias = LocalMcpServer | RemoteMcpServer


@dataclass(frozen=True, slots=True)
class Plugin:
    """A plugin to install: source=None means the plugin registry (by name),
    otherwise source is an absolute path to a local plugin directory."""

    name: str
    source: str | None = None


@dataclass(frozen=True, slots=True)
class Features:
    """Toggles for optional base-image behaviour; gh_auth bootstraps GitHub
    CLI auth, gateway_auth installs the gateway-token auth pair,
    plugin_prune opts into removal of de-specified plugins the base
    installed, mcp_prune likewise for de-specified base-registered MCP
    servers (all default off)."""

    gh_auth: bool = False
    gateway_auth: bool = False
    plugin_prune: bool = False
    mcp_prune: bool = False


@dataclass(frozen=True, slots=True)
class Spec:
    """A fully validated, fully resolved spec.

    Invariants: every string the entrypoint would hand to the openclaw CLI is
    already template-resolved (no {env:...}/{data} tokens survive anywhere);
    lists preserve spec order; absent optional sections resolve to empty
    defaults, never None.
    """

    agent_name: str
    auth_choice: str
    model_fallback: str
    automations_model: str
    automations_default_tools: tuple[str, ...] = ()
    config_entries: list[ConfigEntry] = field(default_factory=list)
    channels: list[Channel] = field(default_factory=list)
    mcp_servers: list[McpServer] = field(default_factory=list)
    plugins: list[Plugin] = field(default_factory=list)
    features: Features = Features()


def mcp_to_cli_args(server: McpServer) -> list[str]:
    """Flags for ``openclaw mcp add NAME <flags...>``; verb and name are the
    caller's job. if_env is not handled here: guard with server.if_env first.
    """
    match server:
        case LocalMcpServer(
            command=command, args=args, env=env, no_probe=no_probe, timeout=timeout
        ):
            flags = ["--command", command]
            for arg in args:
                flags.extend(("--arg", arg))
            for key, value in env.items():
                flags.extend(("--env", f"{key}={value}"))
            if no_probe:
                flags.append("--no-probe")
            if timeout is not None:
                flags.extend(("--timeout", str(timeout)))
            return flags
        case RemoteMcpServer(
            url=url, headers=headers, no_probe=no_probe, timeout=timeout, transport=transport
        ):
            flags = ["--url", url]
            for key, value in headers.items():
                flags.extend(("--header", f"{key}={value}"))
            if transport is not None:
                flags.extend(("--transport", transport))
            if no_probe:
                flags.append("--no-probe")
            if timeout is not None:
                flags.extend(("--timeout", str(timeout)))
            return flags
        case unreachable:
            assert_never(unreachable)


_TOKEN_RE = re.compile(r"\{([^{}]*)\}")
_UNCLOSED_TOKEN_RE = re.compile(r"\{[^{}]*$")
_TOOL_ENTRY_RE = re.compile(r"[a-z0-9_][a-z0-9_.:-]*|\*")


class _DeferredEnv(Mapping[str, str]):
    """Lookup for an if_env-unsatisfied entry: missing vars substitute
    their literal token, so nothing aborts and the inert entry keeps its
    raw shape (it is skipped at reconcile and never applies)."""

    def __init__(self, env: Mapping[str, str]) -> None:
        self._env = env

    def __getitem__(self, key: str) -> str:
        return self._env.get(key, f"{{env:{key}}}")

    def __iter__(self):
        return iter(self._env)

    def __len__(self) -> int:
        return len(self._env)


def _env_for(if_env: tuple[str, ...], env: Mapping[str, str]) -> Mapping[str, str]:
    """Env used to resolve an entry: the real one when its guard is
    satisfied, a deferred one otherwise (optional-secret pattern)."""
    if all(name in env for name in if_env):
        return env
    return _DeferredEnv(env)


_ENV_PREFIX = "env:"


def _fail(path: str, message: str) -> NoReturn:
    raise SpecError(f"{path}: {message}")


def _join(path: str, key: str) -> str:
    return f"{path}.{key}" if path else key


def _resolve_string(value: str, env: Mapping[str, str], path: str) -> str:
    """Substitute {data} and {env:NAME} tokens; any other brace content fails."""

    def substitute(match: re.Match[str]) -> str:
        token = match.group(1)
        if token == "data":
            return str(Path.home() / ".openclaw")
        if token.startswith(_ENV_PREFIX):
            name = token[len(_ENV_PREFIX) :]
            if not name:
                _fail(path, f"invalid template token {{{token}}}: empty variable name")
            if name not in env:
                _fail(path, f"environment variable '{name}' referenced by {{{token}}} is not set")
            return env[name]
        _fail(path, f"unknown template token {{{token}}} (supported: {{env:NAME}}, {{data}})")

    if "{" in value:
        if _UNCLOSED_TOKEN_RE.search(value) is not None:
            _fail(path, "unclosed '{' in template string (tokens are {env:NAME} or {data})")
        return _TOKEN_RE.sub(substitute, value)
    return value


def _resolve_value(value: JSONValue, env: Mapping[str, str], path: str) -> JSONValue:
    """Deep-apply string templating; dict keys and non-string leaves pass through."""
    if isinstance(value, str):
        return _resolve_string(value, env, path)
    if isinstance(value, list):
        return [_resolve_value(item, env, f"{path}[{i}]") for i, item in enumerate(value)]
    if isinstance(value, dict):
        return {key: _resolve_value(item, env, _join(path, key)) for key, item in value.items()}
    return value


def _expect_object(node: object, path: str) -> dict[str, JSONValue]:
    if not isinstance(node, dict):
        _fail(path, "must be a JSON object")
    return node


def _expect_list(node: object, path: str) -> list[JSONValue]:
    if not isinstance(node, list):
        _fail(path, "must be a list")
    return node


def _reject_unknown_keys(node: Mapping[str, JSONValue], allowed: frozenset[str], path: str) -> None:
    for key in node:
        if key not in allowed:
            _fail(_join(path, key), f"unknown key (allowed: {', '.join(sorted(allowed))})")


def _require_key(node: Mapping[str, JSONValue], key: str, path: str) -> JSONValue:
    if key not in node:
        _fail(_join(path, key), "required key is missing")
    return node[key]


def _expect_str(value: object, path: str, *, nonempty: bool = True) -> str:
    if not isinstance(value, str):
        _fail(path, "must be a string")
    if nonempty and not value:
        _fail(path, "must be a non-empty string")
    return value


def _expect_bool(value: object, path: str) -> bool:
    if not isinstance(value, bool):
        _fail(path, "must be a boolean")
    return value


def _expect_int(value: object, path: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool):
        _fail(path, "must be an integer")
    return value


def _expect_str_list(value: object, path: str) -> list[str]:
    return [_expect_str(item, f"{path}[{i}]") for i, item in enumerate(_expect_list(value, path))]


def _expect_str_dict(value: object, path: str) -> dict[str, str]:
    node = _expect_object(value, path)
    return {key: _expect_str(item, _join(path, key), nonempty=False) for key, item in node.items()}


_TOP_LEVEL_KEYS = frozenset(
    {
        "specVersion",
        "agent",
        "setup",
        "model",
        "config",
        "presets",
        "channels",
        "mcp_servers",
        "plugins",
        "features",
        "automations",
    }
)
_AGENT_KEYS = frozenset({"name"})
_SETUP_KEYS = frozenset({"auth_choice"})
_MODEL_KEYS = frozenset({"fallback"})
_CONFIG_ENTRY_KEYS = frozenset({"path", "value", "strict", "if_env", "split_csv"})
_INCLUDE_ONLY_KEYS = frozenset({"include"})
_PRESET_NAME_RE = re.compile(r"[a-z0-9][a-z0-9_-]*")
_CONFIG_KEY_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*")
_CHANNEL_KEYS = frozenset({"type", "use_env"})
_MCP_ENTRY_KEYS = frozenset(
    {
        "name",
        "command",
        "url",
        "args",
        "env",
        "headers",
        "no_probe",
        "timeout",
        "if_env",
        "config",
        "auth",
        "oauth",
        "transport",
    }
)
_MCP_OAUTH_KEYS = frozenset({"identity", "scope", "authProfileId"})
_MCP_TRANSPORT_VALUES = frozenset({"sse", "streamable-http"})
_PLUGIN_KEYS = frozenset({"name", "source"})
_FEATURES_KEYS = frozenset({"gh_auth", "gateway_auth", "plugin_prune", "mcp_prune"})
_AUTOMATIONS_KEYS = frozenset({"model", "default_tools"})


def _parse_presets(root: Mapping[str, JSONValue]) -> dict[str, list[JSONValue]]:
    """Validate the presets table: name → list of config-entry objects.
    Names are lowercase identifier-ish (the include key references them);
    entries are validated as config entries at splice time (a preset may
    only contain plain entries — nesting via include is rejected there)."""
    node = _expect_object(root.get("presets", {}), "presets")
    table: dict[str, list[JSONValue]] = {}
    for name, raw in node.items():
        if _PRESET_NAME_RE.fullmatch(name) is None:
            _fail("presets", f"invalid preset name {name!r} (lowercase letters, digits, - _)")
        entries = _expect_list(raw, f"presets.{name}")
        for index, entry in enumerate(entries):
            base = f"presets.{name}[{index}]"
            entry_obj = _expect_object(entry, base)
            if "include" in entry_obj:
                _fail(base, "presets cannot nest (no 'include' inside a preset)")
            _reject_unknown_keys(entry_obj, _CONFIG_ENTRY_KEYS, base)
        table[name] = entries
    return table


def _expand_config_includes(
    raw_config: list[JSONValue], presets: dict[str, list[JSONValue]]
) -> list[tuple[str, JSONValue]]:
    """Splice {"include": "<name>"} items in place. Nesting (an include
    inside a preset) and unknown names fail closed."""
    expanded: list[tuple[str, JSONValue]] = []
    for index, raw in enumerate(raw_config):
        node = _expect_object(raw, f"config[{index}]")
        if "include" not in node:
            expanded.append((f"config[{index}]", raw))
            continue
        _reject_unknown_keys(node, _INCLUDE_ONLY_KEYS, f"config[{index}]")
        name = node["include"]
        if not isinstance(name, str):
            _fail(f"config[{index}].include", "must be a preset name (string)")
        if name not in presets:
            _fail(f"config[{index}].include", f"unknown preset {name!r}")
        for entry_index, entry in enumerate(presets[name]):
            entry_node = _expect_object(entry, f"preset {name}[{entry_index}]")
            if "include" in entry_node:
                _fail(
                    f"preset {name}[{entry_index}]",
                    "presets cannot nest (no 'include' inside a preset)",
                )
            expanded.append((f"presets.{name}[{entry_index}]", entry))
    return expanded


def _parse_config_entries(
    root: Mapping[str, JSONValue], env: Mapping[str, str]
) -> list[ConfigEntry]:
    presets = _parse_presets(root)
    raw_entries = _expand_config_includes(_expect_list(root.get("config", []), "config"), presets)
    entries: list[ConfigEntry] = []
    for base, raw in raw_entries:
        node = _expect_object(raw, base)
        _reject_unknown_keys(node, _CONFIG_ENTRY_KEYS, base)
        strict = _expect_bool(node.get("strict", False), _join(base, "strict"))
        split_csv = _expect_bool(node.get("split_csv", False), _join(base, "split_csv"))
        if_env = tuple(_expect_str_list(node.get("if_env", []), _join(base, "if_env")))
        lookup = _env_for(if_env, env)
        path = _resolve_string(
            _expect_str(_require_key(node, "path", base), _join(base, "path")),
            lookup,
            _join(base, "path"),
        )
        raw_value: JSONValue = _require_key(node, "value", base)
        resolved = _resolve_value(raw_value, lookup, _join(base, "value"))
        if split_csv:
            if not isinstance(resolved, str):
                _fail(
                    _join(base, "value"),
                    "split_csv requires the resolved value to be a string",
                )
            items = [item.strip() for item in resolved.split(",") if item.strip()]
            if not items:
                names = [
                    token[len(_ENV_PREFIX) :]
                    for token in _TOKEN_RE.findall(raw_value)
                    if token.startswith(_ENV_PREFIX)
                ]
                hint = f" (environment variable {', '.join(names)})" if names else ""
                _fail(_join(base, "value"), f"split_csv produced no non-empty items{hint}")
            cli_value = json.dumps(items)
            use_strict = True
        else:
            use_strict = strict or not isinstance(resolved, str)
            cli_value = json.dumps(resolved) if use_strict else str(resolved)
        entries.append(
            ConfigEntry(
                path=path,
                raw_value=raw_value,
                resolved_value=resolved,
                cli_value=cli_value,
                use_strict_json=use_strict,
                if_env=if_env,
                split_csv=split_csv,
            )
        )
    return entries


def _parse_channels(root: Mapping[str, JSONValue]) -> list[Channel]:
    channels: list[Channel] = []
    for index, raw in enumerate(_expect_list(root.get("channels", []), "channels")):
        base = f"channels[{index}]"
        node = _expect_object(raw, base)
        _reject_unknown_keys(node, _CHANNEL_KEYS, base)
        channels.append(
            Channel(
                type=_expect_str(_require_key(node, "type", base), _join(base, "type")),
                use_env=_expect_bool(node.get("use_env", True), _join(base, "use_env")),
            )
        )
    return channels


def _parse_mcp_auth(
    node: Mapping[str, JSONValue], passthrough_config: dict[str, JSONValue], base: str
) -> tuple[str | None, dict[str, str]]:
    """Parse the first-class 'auth'/'oauth' pair of an mcp_servers entry.

    auth must be "oauth" (the only mode the pinned CLI documents); oauth
    is the metadata object with the documented sub-keys (identity |
    scope | authProfileId), carried verbatim — never templated, because
    the values are structural metadata and OAuth credentials live in
    OpenClaw's own store after a one-time login, never in the spec.
    Passthrough config keys the pair would overwrite ('auth', 'oauth',
    'oauth.*') are a load error instead of a silent precedence rule."""

    for key in passthrough_config:
        if (key == "auth" or key == "oauth" or key.startswith("oauth.")) and (
            "auth" in node or "oauth" in node
        ):
            _fail(
                _join(base, "config"),
                f"key {key!r} conflicts with the first-class 'auth'/'oauth' entry keys",
            )

    if "auth" not in node and "oauth" not in node:
        return None, {}
    if "auth" not in node:
        _fail(base, "'oauth' requires 'auth': \"oauth\"")
    auth = _expect_str(node["auth"], _join(base, "auth"))
    if auth != "oauth":
        _fail(_join(base, "auth"), 'must be "oauth" (the only documented auth mode)')
    oauth: dict[str, str] = {}
    if "oauth" in node:
        raw = _expect_object(node["oauth"], _join(base, "oauth"))
        _reject_unknown_keys(raw, _MCP_OAUTH_KEYS, _join(base, "oauth"))
        for key, value in raw.items():
            oauth[key] = _expect_str(value, _join(f"{base}.oauth", key))
        for key, value in oauth.items():
            if not value:
                _fail(_join(f"{base}.oauth", key), "must be a non-empty string")
            if "{env:" in value or "{data}" in value:
                _fail(
                    _join(f"{base}.oauth", key),
                    "oauth metadata is literal — {env:}/{data} templating is not applied here",
                )
        if oauth.get("identity") not in (None, "shared", "per-requester"):
            _fail(_join(f"{base}.oauth", "identity"), 'must be "shared" or "per-requester"')
        if oauth.get("identity") == "per-requester" and "authProfileId" in oauth:
            _fail(_join(base, "oauth"), "per-requester identity cannot combine with authProfileId")
    return auth, oauth


def _parse_mcp_transport(
    node: Mapping[str, JSONValue],
    passthrough_config: dict[str, JSONValue],
    base: str,
    has_url: bool,
) -> str | None:
    """Parse the first-class 'transport' key of a remote mcp_servers entry.

    transport must be "sse" or "streamable-http" — the pinned CLI's exact
    --transport values (verified at 2026.7.1-2); the CLI default is SSE,
    which POST-only streamable-HTTP endpoints reject (405 on the SSE GET
    handshake). A passthrough config key that would overwrite it
    ('transport') is a load error instead of a silent precedence rule, and
    transport on a local (command) entry is a load error too — the flag is
    HTTP-only."""

    if "transport" in passthrough_config and "transport" in node:
        _fail(
            _join(base, "config"),
            "key 'transport' conflicts with the first-class 'transport' entry key",
        )
    if "transport" not in node:
        return None
    if not has_url:
        _fail(_join(base, "transport"), "'transport' applies to remote (url) servers only")
    transport = _expect_str(node["transport"], _join(base, "transport"))
    if transport not in _MCP_TRANSPORT_VALUES:
        _fail(
            _join(base, "transport"),
            'must be "sse" or "streamable-http" (the pinned CLI\'s --transport values)',
        )
    return transport


def _parse_mcp_servers(root: Mapping[str, JSONValue], env: Mapping[str, str]) -> list[McpServer]:
    servers: list[McpServer] = []
    for index, raw in enumerate(_expect_list(root.get("mcp_servers", []), "mcp_servers")):
        base = f"mcp_servers[{index}]"
        node = _expect_object(raw, base)
        _reject_unknown_keys(node, _MCP_ENTRY_KEYS, base)
        name = _expect_str(_require_key(node, "name", base), _join(base, "name"))
        if_env = tuple(_expect_str_list(node.get("if_env", []), _join(base, "if_env")))
        lookup = _env_for(if_env, env)
        no_probe = _expect_bool(node.get("no_probe", True), _join(base, "no_probe"))
        passthrough_config: dict[str, JSONValue] = {}
        if "config" in node:
            raw_config = _expect_object(node["config"], _join(base, "config"))
            for key, raw_value in raw_config.items():
                if _CONFIG_KEY_RE.fullmatch(key) is None:
                    _fail(
                        _join(base, "config"),
                        f"invalid config key {key!r} (dotted path or identifier)",
                    )
                passthrough_config[key] = _resolve_value(raw_value, lookup, _join(base, "config"))
        has_command = "command" in node
        has_url = "url" in node
        if has_command and has_url:
            _fail(
                base,
                "entry must specify exactly one of 'command' (local) or 'url' (remote), not both",
            )
        if not has_command and not has_url:
            _fail(
                base,
                "entry must specify exactly one of 'command' (local) or "
                "'url' (remote), not neither",
            )
        transport = _parse_mcp_transport(node, passthrough_config, base, has_url)
        auth, oauth = _parse_mcp_auth(node, passthrough_config, base)
        if has_command and (auth is not None or oauth):
            _fail(base, "'auth' and 'oauth' apply to remote (url) servers only")
        if has_url:
            servers.append(
                RemoteMcpServer(
                    name=name,
                    url=_resolve_string(
                        _expect_str(node["url"], _join(base, "url")), lookup, _join(base, "url")
                    ),
                    headers={
                        key: _resolve_string(value, lookup, _join(f"{base}.headers", key))
                        for key, value in _expect_str_dict(
                            node.get("headers", {}), _join(base, "headers")
                        ).items()
                    },
                    no_probe=no_probe,
                    timeout=None
                    if node.get("timeout", None) is None
                    else _expect_int(node.get("timeout"), _join(base, "timeout")),
                    transport=transport,
                    if_env=if_env,
                    passthrough_config=passthrough_config,
                    auth=auth,
                    oauth=oauth,
                )
            )
        else:
            timeout_raw = node.get("timeout", None)
            servers.append(
                LocalMcpServer(
                    name=name,
                    command=_resolve_string(
                        _expect_str(node["command"], _join(base, "command")),
                        lookup,
                        _join(base, "command"),
                    ),
                    args=tuple(
                        _resolve_string(arg, lookup, f"{base}.args[{i}]")
                        for i, arg in enumerate(
                            _expect_str_list(node.get("args", []), _join(base, "args"))
                        )
                    ),
                    env={
                        key: _resolve_string(value, lookup, _join(f"{base}.env", key))
                        for key, value in _expect_str_dict(
                            node.get("env", {}), _join(base, "env")
                        ).items()
                    },
                    no_probe=no_probe,
                    timeout=None
                    if timeout_raw is None
                    else _expect_int(timeout_raw, _join(base, "timeout")),
                    if_env=if_env,
                    passthrough_config=passthrough_config,
                )
            )
    return servers


def _parse_plugins(root: Mapping[str, JSONValue]) -> list[Plugin]:
    plugins: list[Plugin] = []
    for index, raw in enumerate(_expect_list(root.get("plugins", []), "plugins")):
        base = f"plugins[{index}]"
        node = _expect_object(raw, base)
        _reject_unknown_keys(node, _PLUGIN_KEYS, base)
        source: str | None = None
        if "source" in node:
            source = _expect_str(node["source"], _join(base, "source"))
            if not source.startswith("/"):
                _fail(
                    _join(base, "source"),
                    f"local plugin source must be an absolute path, got {source!r}",
                )
        plugins.append(
            Plugin(
                name=_expect_str(_require_key(node, "name", base), _join(base, "name")),
                source=source,
            )
        )
    return plugins


def _parse_features(root: Mapping[str, JSONValue]) -> Features:
    node = _expect_object(root.get("features", {}), "features")
    _reject_unknown_keys(node, _FEATURES_KEYS, "features")
    return Features(
        gh_auth=_expect_bool(node.get("gh_auth", False), "features.gh_auth"),
        gateway_auth=_expect_bool(node.get("gateway_auth", False), "features.gateway_auth"),
        plugin_prune=_expect_bool(node.get("plugin_prune", False), "features.plugin_prune"),
        mcp_prune=_expect_bool(node.get("mcp_prune", False), "features.mcp_prune"),
    )


_ZAI_AUTH_PREFIX = "zai-coding-"


def required_env_for_auth_choice(auth_choice: str) -> str | None:
    """Env var the openclaw setup CLI consumes for this auth choice, if any.

    zai-coding-* choices authenticate against Z.AI (GLM Coding Plan) with
    ZAI_API_KEY; setup exits 1 without it — after installing its plugin —
    and leaves no openclaw.json behind, so an ungated boot crash-loops."""
    if auth_choice.startswith(_ZAI_AUTH_PREFIX):
        return "ZAI_API_KEY"
    return None


def load_spec(path: Path, env: Mapping[str, str]) -> Spec:
    """Load, validate, and template-resolve a spec.json file.

    env is the only environment consulted (never os.environ). Raises SpecError
    (a ValueError) with a JSON-path-prefixed message on any violation; raises
    OSError unchanged when the file cannot be read.
    """
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise SpecError(f"{path}: not valid JSON ({exc})") from exc

    root = _expect_object(raw, "$")

    # Version gate comes first so future specs fail fast with a clear
    # version error instead of cascading unknown-key noise.
    version = _expect_int(_require_key(root, "specVersion", ""), "specVersion")
    if version != SPEC_VERSION_SUPPORTED:
        _fail(
            "specVersion",
            f"spec version {version} is not supported (this loader "
            f"supports specVersion {SPEC_VERSION_SUPPORTED})",
        )

    _reject_unknown_keys(root, _TOP_LEVEL_KEYS, "")

    agent = _expect_object(_require_key(root, "agent", ""), "agent")
    _reject_unknown_keys(agent, _AGENT_KEYS, "agent")
    setup = _expect_object(_require_key(root, "setup", ""), "setup")
    _reject_unknown_keys(setup, _SETUP_KEYS, "setup")
    model = _expect_object(_require_key(root, "model", ""), "model")
    _reject_unknown_keys(model, _MODEL_KEYS, "model")
    automations = _expect_object(_require_key(root, "automations", ""), "automations")
    _reject_unknown_keys(automations, _AUTOMATIONS_KEYS, "automations")

    agent_name = _expect_str(_require_key(agent, "name", "agent"), "agent.name")
    auth_choice = _expect_str(_require_key(setup, "auth_choice", "setup"), "setup.auth_choice")
    model_fallback = _expect_str(_require_key(model, "fallback", "model"), "model.fallback")
    automations_model = _expect_str(
        _require_key(automations, "model", "automations"), "automations.model"
    )
    default_tools_raw = automations.get("default_tools", [])
    bad_default_tools = not isinstance(default_tools_raw, list) or not default_tools_raw
    if "default_tools" in automations and bad_default_tools:
        _fail("automations.default_tools", "must be a non-empty list of tool names")
    for token in default_tools_raw:
        if not isinstance(token, str) or not _TOOL_ENTRY_RE.fullmatch(token):
            _fail(
                "automations.default_tools",
                f"invalid tool entry {token!r} (tool names or * for unrestricted)",
            )
    automations_default_tools = tuple(default_tools_raw)
    config_entries = _parse_config_entries(root, env)
    channels = _parse_channels(root)
    mcp_servers = _parse_mcp_servers(root, env)
    plugins = _parse_plugins(root)
    features = _parse_features(root)

    # Runs after template resolution on purpose: {env:...} errors keep their
    # documented precedence (locked by AuthChoiceEnvGate).
    required_env = required_env_for_auth_choice(auth_choice)
    if required_env is not None and required_env not in env:
        _fail(
            "setup.auth_choice",
            f"auth choice '{auth_choice}' requires environment variable {required_env} "
            "(set it and restart)",
        )

    return Spec(
        agent_name=agent_name,
        auth_choice=auth_choice,
        model_fallback=model_fallback,
        automations_model=automations_model,
        automations_default_tools=automations_default_tools,
        config_entries=config_entries,
        channels=channels,
        mcp_servers=mcp_servers,
        plugins=plugins,
        features=features,
    )
