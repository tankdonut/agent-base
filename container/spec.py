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


@dataclass(frozen=True, slots=True)
class RemoteMcpServer:
    """An HTTP MCP server (``openclaw mcp add NAME --url ...``).

    Invariants: url and header values are template-resolved at load time
    ({env:...} for API keys is the intended use); headers preserve insertion
    order; timeout is seconds (CLI contract, verified at 2026.7.1-2).
    """

    name: str
    url: str
    headers: dict[str, str] = field(default_factory=dict)
    no_probe: bool = True
    timeout: int | None = None
    if_env: tuple[str, ...] = ()


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
    CLI auth at startup (default off)."""

    gh_auth: bool = False


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
        case RemoteMcpServer(url=url, headers=headers, no_probe=no_probe, timeout=timeout):
            flags = ["--url", url]
            for key, value in headers.items():
                flags.extend(("--header", f"{key}={value}"))
            if no_probe:
                flags.append("--no-probe")
            if timeout is not None:
                flags.extend(("--timeout", str(timeout)))
            return flags
        case unreachable:
            assert_never(unreachable)


_TOKEN_RE = re.compile(r"\{([^{}]*)\}")
_UNCLOSED_TOKEN_RE = re.compile(r"\{[^{}]*$")
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
_CHANNEL_KEYS = frozenset({"type", "use_env"})
_MCP_ENTRY_KEYS = frozenset(
    {"name", "command", "url", "args", "env", "headers", "no_probe", "timeout", "if_env"}
)
_PLUGIN_KEYS = frozenset({"name", "source"})
_FEATURES_KEYS = frozenset({"gh_auth"})
_AUTOMATIONS_KEYS = frozenset({"model"})


def _parse_config_entries(
    root: Mapping[str, JSONValue], env: Mapping[str, str]
) -> list[ConfigEntry]:
    entries: list[ConfigEntry] = []
    for index, raw in enumerate(_expect_list(root.get("config", []), "config")):
        base = f"config[{index}]"
        node = _expect_object(raw, base)
        _reject_unknown_keys(node, _CONFIG_ENTRY_KEYS, base)
        path = _expect_str(_require_key(node, "path", base), _join(base, "path"))
        raw_value: JSONValue = _require_key(node, "value", base)
        strict = _expect_bool(node.get("strict", False), _join(base, "strict"))
        split_csv = _expect_bool(node.get("split_csv", False), _join(base, "split_csv"))
        if_env = tuple(_expect_str_list(node.get("if_env", []), _join(base, "if_env")))
        resolved = _resolve_value(raw_value, env, _join(base, "value"))
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


def _parse_mcp_servers(root: Mapping[str, JSONValue], env: Mapping[str, str]) -> list[McpServer]:
    servers: list[McpServer] = []
    for index, raw in enumerate(_expect_list(root.get("mcp_servers", []), "mcp_servers")):
        base = f"mcp_servers[{index}]"
        node = _expect_object(raw, base)
        _reject_unknown_keys(node, _MCP_ENTRY_KEYS, base)
        name = _expect_str(_require_key(node, "name", base), _join(base, "name"))
        if_env = tuple(_expect_str_list(node.get("if_env", []), _join(base, "if_env")))
        no_probe = _expect_bool(node.get("no_probe", True), _join(base, "no_probe"))
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
        if has_url:
            servers.append(
                RemoteMcpServer(
                    name=name,
                    url=_resolve_string(
                        _expect_str(node["url"], _join(base, "url")), env, _join(base, "url")
                    ),
                    headers={
                        key: _resolve_string(value, env, _join(f"{base}.headers", key))
                        for key, value in _expect_str_dict(
                            node.get("headers", {}), _join(base, "headers")
                        ).items()
                    },
                    no_probe=no_probe,
                    timeout=None
                    if node.get("timeout", None) is None
                    else _expect_int(node.get("timeout"), _join(base, "timeout")),
                    if_env=if_env,
                )
            )
        else:
            timeout_raw = node.get("timeout", None)
            servers.append(
                LocalMcpServer(
                    name=name,
                    command=_resolve_string(
                        _expect_str(node["command"], _join(base, "command")),
                        env,
                        _join(base, "command"),
                    ),
                    args=tuple(
                        _resolve_string(arg, env, f"{base}.args[{i}]")
                        for i, arg in enumerate(
                            _expect_str_list(node.get("args", []), _join(base, "args"))
                        )
                    ),
                    env={
                        key: _resolve_string(value, env, _join(f"{base}.env", key))
                        for key, value in _expect_str_dict(
                            node.get("env", {}), _join(base, "env")
                        ).items()
                    },
                    no_probe=no_probe,
                    timeout=None
                    if timeout_raw is None
                    else _expect_int(timeout_raw, _join(base, "timeout")),
                    if_env=if_env,
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
    return Features(gh_auth=_expect_bool(node.get("gh_auth", False), "features.gh_auth"))


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
        config_entries=config_entries,
        channels=channels,
        mcp_servers=mcp_servers,
        plugins=plugins,
        features=features,
    )
