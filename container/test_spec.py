#!/usr/bin/env python3
"""Self-contained unittest suite for the spec.json loader (container/spec.py).

Covers the schema v1 contract: fail-closed unknown-key rejection at every
level, required sections/fields, specVersion gating, {env:NAME}/{data}
templating (including nested values and single-pass substitution),
pre-derived CLI marshalling for config entries, local/remote MCP server
validation and flag construction, plugin sources, if_env guards, and the
golden templates/spec.example.json.

Runs directly with no pytest dependency:

    python3 -m unittest discover -s container -p "test_spec.py" -v

The loader is fail-closed: every violation raises SpecError with the JSON
path of the offending node prefixed to the message, so most assertions here
check ``str(ctx.exception)`` for that path.
"""

from __future__ import annotations

import copy
import json
import os
import tempfile
import unittest
from collections.abc import Mapping
from pathlib import Path
from typing import cast
from unittest import mock

from spec import (
    SPEC_VERSION_SUPPORTED,
    Features,
    LocalMcpServer,
    Plugin,
    RemoteMcpServer,
    Spec,
    SpecError,
    load_spec,
    mcp_to_cli_args,
)

REPO_ROOT = Path(__file__).resolve().parent.parent
EXAMPLE_SPEC = REPO_ROOT / "templates" / "spec.example.json"

MINIMAL: dict[str, object] = {
    "specVersion": 1,
    "agent": {"name": "t-agent"},
    "setup": {"auth_choice": "zai-coding-global"},
    "model": {"fallback": "zai/glm-4.7"},
    "automations": {"model": "zai/glm-4.7"},
}

BASE_ENV: Mapping[str, str] = {
    "GREETING": "hello",
    "TOKEN": "tok",
    "SECRET": "s3cret",
    "ZAI_API_KEY": "zai-key",
}

GOLDEN_ENV: Mapping[str, str] = {
    "TELEGRAM_BOT_TOKEN": "tg-token-123",
    "OPENCLAW_GATEWAY_TOKEN": "gw-token-456",
    "AC_INFINITY_EMAIL": "grower@example.com",
    "AC_INFINITY_PASSWORD": "s3cret",
    "SENTIMENT_API_KEY": "sk-sentiment",
    "ZAI_API_KEY": "zai-key",
}


def minimal_without(key_path: str) -> dict[str, object]:
    """A deep copy of MINIMAL with one dotted key removed."""
    spec = copy.deepcopy(MINIMAL)
    if "." in key_path:
        outer, inner = key_path.split(".", 1)
        cast(dict[str, object], spec[outer]).pop(inner)
    else:
        spec.pop(key_path)
    return spec


class SpecTestCase(unittest.TestCase):
    """Shared harness: writes spec dicts to a tmpdir and loads them."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)

    def load(self, spec: object, env: Mapping[str, str] | None = None) -> Spec:
        path = self.tmp / "spec.json"
        path.write_text(json.dumps(spec), encoding="utf-8")
        return load_spec(path, BASE_ENV if env is None else env)

    def load_expect_error(
        self, spec: object, env: Mapping[str, str] | None = None, containing: str | None = None
    ) -> str:
        with self.assertRaises(SpecError) as ctx:
            self.load(spec, env)
        message = str(ctx.exception)
        if containing is not None:
            self.assertIn(containing, message)
        return message

    def config_spec(self, value: object, **extra: object) -> dict[str, object]:
        spec = copy.deepcopy(MINIMAL)
        entry: dict[str, object] = {"path": "some.key", "value": value, **extra}
        spec["config"] = [entry]
        return spec


class GoldenExampleSpec(SpecTestCase):
    def test_golden_example_validates_with_full_env(self) -> None:
        spec = load_spec(EXAMPLE_SPEC, GOLDEN_ENV)

        self.assertEqual("example-agent", spec.agent_name)
        self.assertEqual("zai-coding-global", spec.auth_choice)
        self.assertEqual("zai/glm-4.7", spec.model_fallback)
        self.assertEqual("zai/glm-4.7", spec.automations_model)
        self.assertEqual(Features(gh_auth=True, gateway_auth=True), spec.features)

        by_path = {entry.path: entry for entry in spec.config_entries}
        token = by_path["telegram.botToken"]
        self.assertEqual("tg-token-123", token.resolved_value)
        self.assertEqual('"tg-token-123"', token.cli_value)
        self.assertTrue(token.use_strict_json)

        home = str(Path.home() / ".openclaw")
        dirs = by_path["permissions.additionalDirectories"]
        self.assertEqual([f"{home}/workspace", f"{home}/skills"], dirs.resolved_value)

        servers = {server.name: server for server in spec.mcp_servers}
        filesystem = servers["filesystem"]
        self.assertIsInstance(filesystem, LocalMcpServer)
        self.assertEqual(f"{home}/bin/filesystem-mcp", filesystem.command)
        self.assertEqual(("--root", f"{home}/workspace"), filesystem.args)

        ac = servers["ac-infinity"]
        self.assertIsInstance(ac, LocalMcpServer)
        self.assertEqual(30, ac.timeout)
        self.assertFalse(ac.no_probe)
        self.assertEqual(
            [
                "--command",
                "ac-infinity-mcp",
                "--env",
                "AC_INFINITY_EMAIL=grower@example.com",
                "--env",
                "AC_INFINITY_PASSWORD=s3cret",
                "--timeout",
                "30",
            ],
            mcp_to_cli_args(ac),
        )

        sentiment = servers["sentiment"]
        self.assertIsInstance(sentiment, RemoteMcpServer)
        self.assertEqual("https://mcp.example.com/sentiment", sentiment.url)
        self.assertEqual({"Authorization": "Bearer sk-sentiment"}, sentiment.headers)
        self.assertEqual(("SENTIMENT_API_KEY",), sentiment.if_env)

        self.assertEqual(
            [("approvals", None), ("internal-tools", "/opt/agent/plugins/internal-tools")],
            [(plugin.name, plugin.source) for plugin in spec.plugins],
        )
        self.assertEqual("telegram", spec.channels[0].type)
        self.assertTrue(spec.channels[0].use_env)

    def test_golden_example_fails_closed_without_env(self) -> None:
        with self.assertRaises(SpecError) as ctx:
            load_spec(EXAMPLE_SPEC, {})
        self.assertIn("TELEGRAM_BOT_TOKEN", str(ctx.exception))


class SpecVersionGate(SpecTestCase):
    def test_missing_specversion_is_rejected(self) -> None:
        self.load_expect_error(minimal_without("specVersion"), containing="specVersion")

    def test_specversion_1_is_accepted(self) -> None:
        self.assertEqual(1, SPEC_VERSION_SUPPORTED)
        spec = self.load(MINIMAL)
        self.assertEqual("t-agent", spec.agent_name)

    def test_newer_specversion_names_both_versions(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["specVersion"] = SPEC_VERSION_SUPPORTED + 1
        message = self.load_expect_error(spec, containing="specVersion")
        self.assertIn(str(SPEC_VERSION_SUPPORTED + 1), message)
        self.assertIn(str(SPEC_VERSION_SUPPORTED), message)

    def test_non_integer_specversion_is_rejected(self) -> None:
        for bad in ("1", 1.0, True, None):
            with self.subTest(bad=bad):
                spec = copy.deepcopy(MINIMAL)
                spec["specVersion"] = bad
                self.load_expect_error(spec, containing="specVersion")


class UnknownKeyRejection(SpecTestCase):
    def test_unknown_top_level_key(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["confog"] = []
        self.load_expect_error(spec, containing="confog:")

    def test_unknown_key_inside_agent(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["agent"]["nmae"] = "x"
        self.load_expect_error(spec, containing="agent.nmae:")

    def test_unknown_key_inside_config_entry(self) -> None:
        spec = self.config_spec("v", vaule=1)
        self.load_expect_error(spec, containing="config[0].vaule:")

    def test_unknown_key_inside_mcp_entry(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [{"name": "x", "command": "c", "prboe": False}]
        self.load_expect_error(spec, containing="mcp_servers[0].prboe:")

    def test_unknown_key_inside_features(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["features"] = {"gh_auth": True, "ghAuth": True}
        self.load_expect_error(spec, containing="features.ghAuth:")


class RequiredSectionsAndFields(SpecTestCase):
    def test_missing_required_sections_and_fields(self) -> None:
        for key_path in (
            "specVersion",
            "agent",
            "agent.name",
            "setup",
            "setup.auth_choice",
            "model",
            "model.fallback",
            "automations",
            "automations.model",
        ):
            with self.subTest(missing=key_path):
                self.load_expect_error(minimal_without(key_path), containing=key_path)

    def test_missing_value_in_config_entry(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["config"] = [{"path": "some.key"}]
        self.load_expect_error(spec, containing="config[0].value")

    def test_missing_name_in_mcp_entry(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [{"command": "c"}]
        self.load_expect_error(spec, containing="mcp_servers[0].name")

    def test_missing_type_in_channel(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["channels"] = [{"use_env": False}]
        self.load_expect_error(spec, containing="channels[0].type")


class AuthChoiceEnvGate(SpecTestCase):
    """zai-coding-* auth choices consume ZAI_API_KEY directly at setup time;
    the loader must fail closed naming the var before any container work."""

    def test_zai_auth_choice_requires_zai_api_key(self) -> None:
        message = self.load_expect_error(MINIMAL, env={}, containing="ZAI_API_KEY")
        self.assertIn("setup.auth_choice", message)

    def test_zai_api_key_present_loads(self) -> None:
        spec = self.load(MINIMAL, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual("zai-coding-global", spec.auth_choice)

    def test_zai_coding_cn_variant_also_requires_the_key(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["setup"] = {"auth_choice": "zai-coding-cn"}
        self.load_expect_error(variant, env={}, containing="ZAI_API_KEY")

    def test_non_zai_auth_choice_has_no_env_requirement(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["setup"] = {"auth_choice": "manual"}
        spec = self.load(variant, env={})
        self.assertEqual("manual", spec.auth_choice)

    def test_template_errors_take_precedence_over_auth_gate(self) -> None:
        # Locks the fail order: {env:...} resolution fires before the gate,
        # matching the documented error-precedence of the golden example.
        message = self.load_expect_error(
            self.config_spec("{env:MISSING_VAR}"), env={}, containing="MISSING_VAR"
        )
        self.assertNotIn("ZAI_API_KEY", message)


class AutomationsDefaultTools(SpecTestCase):
    """automations.default_tools (optional): overrides the base's bounded
    tool allow-list for seeded jobs that don't declare their own `tools:`."""

    def test_absent_key_loads_with_empty_tuple(self) -> None:
        spec = self.load(MINIMAL, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual((), spec.automations_default_tools)

    def test_valid_list_accepted(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["automations"] = {
            "model": "zai/glm-4.7",
            "default_tools": ["read", "exec", "bundle-mcp"],
        }
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual(("read", "exec", "bundle-mcp"), spec.automations_default_tools)

    def test_star_means_unrestricted(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["automations"] = {"model": "zai/glm-4.7", "default_tools": ["*"]}
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual(("*",), spec.automations_default_tools)

    def test_malformed_values_fail_closed(self) -> None:
        for bad in ([], ["read", ""], ["read write"], [42], "read"):
            with self.subTest(bad=bad):
                variant = copy.deepcopy(MINIMAL)
                variant["automations"] = {"model": "zai/glm-4.7", "default_tools": bad}
                self.load_expect_error(
                    variant, env={"ZAI_API_KEY": "zai-key"}, containing="default_tools"
                )


class PluginPruneFeature(SpecTestCase):
    """features.plugin_prune (default false): opt-in removal of de-specified
    plugins the base itself installed (ownership via
    {data}/agent-managed-spec-plugins)."""

    def test_absent_defaults_false(self) -> None:
        spec = self.load(MINIMAL, env={"ZAI_API_KEY": "zai-key"})
        self.assertFalse(spec.features.plugin_prune)

    def test_true_accepted(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["features"] = {"gh_auth": False, "plugin_prune": True}
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertTrue(spec.features.plugin_prune)


class ConfigPresets(SpecTestCase):
    """presets: named config-entry groups spliced into config in place via
    {"include": "<name>"} — the ~10 duplicated baseline entries across
    consumers collapse to one name. Fail-closed: unknown name, nesting,
    and malformed shapes are load errors."""

    def _with_presets(self, presets: object, config: list[object]) -> dict[str, object]:
        variant = copy.deepcopy(MINIMAL)
        variant["presets"] = presets
        variant["config"] = config
        return variant

    def test_include_splices_entries_in_place(self) -> None:
        spec = self.load(
            self._with_presets(
                {
                    "telegram-baseline": [
                        {"path": "channels.telegram.dmPolicy", "value": "allowlist"},
                        {"path": "agents.defaults.heartbeat.target", "value": "telegram"},
                    ]
                },
                [
                    {"include": "telegram-baseline"},
                    {"path": "z.last", "value": "1"},
                ],
            ),
            env={"ZAI_API_KEY": "zai-key"},
        )
        self.assertEqual(
            [
                "channels.telegram.dmPolicy",
                "agents.defaults.heartbeat.target",
                "z.last",
            ],
            [e.path for e in spec.config_entries],
        )

    def test_unknown_include_fails_closed(self) -> None:
        variant = self._with_presets(
            {"known": [{"path": "a.b", "value": 1}]},
            [{"include": "nope"}],
        )
        message = self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="nope")
        self.assertIn("preset", message)

    def test_include_inside_preset_fails_closed(self) -> None:
        variant = self._with_presets(
            {"outer": [{"include": "inner"}], "inner": [{"path": "a.b", "value": 1}]},
            [{"include": "outer"}],
        )
        self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="nest")

    def test_include_with_extra_keys_fails_closed(self) -> None:
        variant = self._with_presets(
            {"p": [{"path": "a.b", "value": 1}]},
            [{"include": "p", "path": "x.y", "value": 2}],
        )
        self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="include")

    def test_preset_entries_get_full_treatment(self) -> None:
        # Templating, if_env deferral, split_csv — identical to inline
        # entries once spliced.
        spec = self.load(
            self._with_presets(
                {
                    "p": [
                        {
                            "path": "channels.telegram.allowFrom",
                            "value": "{env:TELEGRAM_ALLOWED_USERS}",
                            "split_csv": True,
                        },
                        {
                            "path": "agents.defaults.heartbeat.to",
                            "value": "{env:TELEGRAM_CHAT_ID}",
                            "if_env": ["TELEGRAM_CHAT_ID"],
                        },
                    ]
                },
                [{"include": "p"}],
            ),
            env={"ZAI_API_KEY": "zai-key", "TELEGRAM_ALLOWED_USERS": "1, 2"},
        )
        self.assertEqual('["1", "2"]', spec.config_entries[0].cli_value)
        self.assertEqual("{env:TELEGRAM_CHAT_ID}", spec.config_entries[1].resolved_value)

    def test_bad_preset_name_rejected(self) -> None:
        variant = self._with_presets(
            {"bad name!": [{"path": "a.b", "value": 1}]},
            [],
        )
        self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="bad name")

    def test_preset_non_list_rejected(self) -> None:
        variant = self._with_presets({"p": {"path": "a.b"}}, [])
        self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="p")

    def test_presets_key_absent_is_fine(self) -> None:
        spec = self.load(MINIMAL, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual(0, len(spec.config_entries))


class PathTemplating(SpecTestCase):
    """Config paths accept {env:...} tokens (chat IDs stop being baked
    into git). Resolution mirrors values: fail-closed when unguarded,
    deferred when the entry's if_env guard is unsatisfied at load."""

    def test_path_tokens_resolve(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["config"] = [
            {
                "path": "channels.telegram.groups.{env:TELEGRAM_GROUP_ID}.enabled",
                "value": True,
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key", "TELEGRAM_GROUP_ID": "-10042"})
        self.assertEqual("channels.telegram.groups.-10042.enabled", spec.config_entries[0].path)

    def test_missing_path_var_fails_closed_naming_var(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["config"] = [
            {"path": "channels.telegram.groups.{env:TELEGRAM_GROUP_ID}.enabled", "value": True}
        ]
        message = self.load_expect_error(
            variant, env={"ZAI_API_KEY": "zai-key"}, containing="TELEGRAM_GROUP_ID"
        )
        self.assertIn("path", message)

    def test_guarded_entry_with_unresolvable_path_loads_inert(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["config"] = [
            {
                "path": "channels.telegram.groups.{env:TELEGRAM_GROUP_ID}.enabled",
                "value": True,
                "if_env": ["TELEGRAM_GROUP_ID"],
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual(1, len(spec.config_entries))


class OptionalSecrets(SpecTestCase):
    """if_env-guarded entries are optional: when the guard is unsatisfied
    at load, value resolution is deferred (raw tokens preserved, entry
    inert) instead of aborting the boot on the missing var."""

    def test_guarded_entry_missing_var_loads_and_stays_raw(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["config"] = [
            {
                "path": "approvals.plugin.mode",
                "value": "{env:OPTIONAL_TOKEN}",
                "if_env": ["OPTIONAL_TOKEN"],
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual("{env:OPTIONAL_TOKEN}", spec.config_entries[0].resolved_value)

    def test_guard_satisfied_missing_var_still_fails_closed(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["config"] = [
            {
                "path": "agents.defaults.heartbeat.to",
                "value": "{env:TELEGRAM_CHAT_ID}",
                "if_env": ["TELEGRAM_BOT_TOKEN"],
            }
        ]
        self.load_expect_error(
            variant,
            env={"ZAI_API_KEY": "zai-key", "TELEGRAM_BOT_TOKEN": "bot-1"},
            containing="TELEGRAM_CHAT_ID",
        )

    def test_unguarded_missing_var_still_fails_closed(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["config"] = [{"path": "x.y", "value": "{env:MISSING_ONE}"}]
        self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="MISSING_ONE")

    def test_guarded_mcp_server_missing_key_loads(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [
            {
                "name": "optional-remote",
                "url": "https://mcp.example.com/s",
                "headers": {"Authorization": "Bearer {env:OPTIONAL_MCP_KEY}"},
                "if_env": ["OPTIONAL_MCP_KEY"],
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual(1, len(spec.mcp_servers))
        server = spec.mcp_servers[0]
        self.assertIsInstance(server, RemoteMcpServer)
        self.assertEqual("Bearer {env:OPTIONAL_MCP_KEY}", server.headers["Authorization"])

    def test_guarded_mcp_server_satisfied_resolves(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [
            {
                "name": "optional-remote",
                "url": "https://mcp.example.com/s",
                "headers": {"Authorization": "Bearer {env:OPTIONAL_MCP_KEY}"},
                "if_env": ["OPTIONAL_MCP_KEY"],
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key", "OPTIONAL_MCP_KEY": "sk-1"})
        server = spec.mcp_servers[0]
        self.assertIsInstance(server, RemoteMcpServer)
        self.assertEqual("Bearer sk-1", server.headers["Authorization"])


class McpPassThrough(SpecTestCase):
    """mcp_servers[].config: arbitrary per-server knobs (requestTimeoutMs,
    toolFilter, oauth.identity, ...) applied as mcp.servers.<name>.<key>
    via config set --strict-json after registration — the escape hatch for
    knobs the frozen v1 entry shape doesn't name. Keys are the runtime's
    schema's business: an invalid key fails the (warn-only) config set
    visibly rather than at load."""

    def test_config_object_accepted_and_templated(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [
            {
                "name": "slow",
                "url": "https://mcp.example.com/s",
                "config": {
                    "requestTimeoutMs": 45000,
                    "transport": "streamable-http",
                    "oauth.identity": "shared",
                },
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual(
            {"requestTimeoutMs": 45000, "transport": "streamable-http", "oauth.identity": "shared"},
            spec.mcp_servers[0].passthrough_config,
        )

    def test_config_string_value_templated(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [
            {
                "name": "authed",
                "url": "https://mcp.example.com/s",
                "config": {"oauth.token": "{env:MCP_OAUTH_TOKEN}"},
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key", "MCP_OAUTH_TOKEN": "tok-1"})
        self.assertEqual({"oauth.token": "tok-1"}, spec.mcp_servers[0].passthrough_config)

    def test_config_non_object_rejected(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [{"name": "x", "url": "https://x", "config": [1, 2]}]
        self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="config")

    def test_config_bad_key_shape_rejected(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [{"name": "x", "url": "https://x", "config": {"": 1}}]
        self.load_expect_error(variant, env={"ZAI_API_KEY": "zai-key"}, containing="config")

    def test_unguarded_config_var_missing_fails_closed(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [
            {
                "name": "authed",
                "url": "https://mcp.example.com/s",
                "config": {"oauth.token": "{env:MCP_REQUIRED_TOKEN}"},
            }
        ]
        self.load_expect_error(
            variant, env={"ZAI_API_KEY": "zai-key"}, containing="MCP_REQUIRED_TOKEN"
        )

    def test_guarded_config_var_missing_defers(self) -> None:
        variant = copy.deepcopy(MINIMAL)
        variant["mcp_servers"] = [
            {
                "name": "authed",
                "url": "https://mcp.example.com/s",
                "config": {"oauth.token": "{env:MCP_OPTIONAL_TOKEN}"},
                "if_env": ["MCP_OPTIONAL_TOKEN"],
            }
        ]
        spec = self.load(variant, env={"ZAI_API_KEY": "zai-key"})
        self.assertEqual(
            {"oauth.token": "{env:MCP_OPTIONAL_TOKEN}"},
            spec.mcp_servers[0].passthrough_config,
        )


class TemplateResolution(SpecTestCase):
    def test_env_token_resolved_inline(self) -> None:
        spec = self.load(self.config_spec("Hello {env:GREETING} world"))
        entry = spec.config_entries[0]
        self.assertEqual("Hello hello world", entry.resolved_value)
        self.assertEqual("Hello hello world", entry.cli_value)
        self.assertFalse(entry.use_strict_json)

    def test_missing_env_var_error_names_variable(self) -> None:
        message = self.load_expect_error(
            self.config_spec("{env:MISSING_VAR}"),
            env={"GREETING": "x"},
            containing="config[0].value",
        )
        self.assertIn("MISSING_VAR", message)

    def test_process_environ_is_never_consulted(self) -> None:
        # A var present only in os.environ must NOT satisfy the template:
        # env comes exclusively from the mapping passed to load_spec.
        with (
            mock.patch.dict(os.environ, {"MISSING_VAR": "leaked"}),
            self.assertRaises(SpecError) as ctx,
        ):
            self.load(self.config_spec("{env:MISSING_VAR}"), env={})
        self.assertIn("MISSING_VAR", str(ctx.exception))

    def test_data_token_resolves_to_openclaw_home(self) -> None:
        spec = self.load(self.config_spec("{data}/workspace"))
        self.assertEqual(
            str(Path.home() / ".openclaw" / "workspace"),
            spec.config_entries[0].resolved_value,
        )

    def test_unknown_token_is_rejected(self) -> None:
        message = self.load_expect_error(
            self.config_spec("{foo:bar}"), containing="config[0].value"
        )
        self.assertIn("{foo:bar}", message)

    def test_unclosed_brace_is_rejected(self) -> None:
        self.load_expect_error(self.config_spec("x {env:GREETING"), containing="config[0].value")

    def test_templating_inside_nested_list(self) -> None:
        spec = self.load(self.config_spec(["{env:GREETING}", ["{data}"], 3]))
        self.assertEqual(
            ["hello", [str(Path.home() / ".openclaw")], 3],
            spec.config_entries[0].resolved_value,
        )

    def test_templating_inside_nested_dict(self) -> None:
        spec = self.load(self.config_spec({"a": "{env:GREETING}", "b": {"c": "{data}"}, "d": 1}))
        self.assertEqual(
            {"a": "hello", "b": {"c": str(Path.home() / ".openclaw")}, "d": 1},
            spec.config_entries[0].resolved_value,
        )

    def test_substitution_is_single_pass(self) -> None:
        # A substituted value that itself looks like a token stays literal.
        spec = self.load(
            self.config_spec("{env:TEMPLATE_VAR}"),
            env={"TEMPLATE_VAR": "{data}", "ZAI_API_KEY": "zai-key"},
        )
        self.assertEqual("{data}", spec.config_entries[0].resolved_value)

    def test_raw_value_preserves_template_tokens(self) -> None:
        spec = self.load(self.config_spec("{env:GREETING}"))
        self.assertEqual("{env:GREETING}", spec.config_entries[0].raw_value)


class ConfigMarshalling(SpecTestCase):
    def test_cli_value_and_strict_flag_matrix(self) -> None:
        cases = [
            ("plain string stays bare", {"value": "hello"}, "hello", False),
            ("strict string is quoted json", {"value": "hello", "strict": True}, '"hello"', True),
            ("bool true marshals to json", {"value": True}, "true", True),
            ("bool false marshals to json", {"value": False}, "false", True),
            ("int 20 marshals as 20", {"value": 20}, "20", True),
            ("float marshals as json", {"value": 0.5}, "0.5", True),
            ("list marshals as json", {"value": ["a", 1]}, '["a", 1]', True),
            ("null marshals as json", {"value": None}, "null", True),
        ]
        for label, extra, cli_value, use_strict in cases:
            with self.subTest(case=label):
                spec = self.load(
                    self.config_spec(
                        extra["value"], **{k: v for k, v in extra.items() if k != "value"}
                    )
                )
                entry = spec.config_entries[0]
                self.assertEqual(cli_value, entry.cli_value)
                self.assertEqual(use_strict, entry.use_strict_json)

    def test_config_entry_defaults(self) -> None:
        spec = self.load(self.config_spec("v"))
        entry = spec.config_entries[0]
        self.assertFalse(entry.use_strict_json)
        self.assertEqual((), entry.if_env)
        self.assertFalse(entry.split_csv)


class SplitCsvConfig(SpecTestCase):
    CSV_ENV: Mapping[str, str] = {"CSV_LIST": " 111 , 222 ,, 333 ", "ZAI_API_KEY": "zai-key"}

    def test_split_csv_strips_items_and_drops_empties(self) -> None:
        spec = self.load(self.config_spec("{env:CSV_LIST}", split_csv=True), env=self.CSV_ENV)
        entry = spec.config_entries[0]
        self.assertEqual('["111", "222", "333"]', entry.cli_value)
        self.assertTrue(entry.use_strict_json)
        self.assertTrue(entry.split_csv)
        # resolved_value keeps the substituted string; only cli_value is the list.
        self.assertEqual(" 111 , 222 ,, 333 ", entry.resolved_value)

    def test_split_csv_without_env_splits_literal_string(self) -> None:
        spec = self.load(self.config_spec("x, y", split_csv=True))
        entry = spec.config_entries[0]
        self.assertEqual('["x", "y"]', entry.cli_value)
        self.assertTrue(entry.use_strict_json)

    def test_split_csv_single_item_has_no_comma(self) -> None:
        spec = self.load(self.config_spec("solo", split_csv=True))
        self.assertEqual('["solo"]', spec.config_entries[0].cli_value)

    def test_split_csv_empty_result_fails_closed_naming_env_var(self) -> None:
        message = self.load_expect_error(
            self.config_spec("{env:CSV_LIST}", split_csv=True),
            env={"CSV_LIST": " , , "},
            containing="config[0].value",
        )
        self.assertIn("CSV_LIST", message)

    def test_split_csv_empty_literal_result_fails_closed_too(self) -> None:
        # No env var to name — the error must still fire, naming the path.
        self.load_expect_error(
            self.config_spec(" , ", split_csv=True), containing="config[0].value"
        )

    def test_split_csv_non_string_resolved_value_is_rejected(self) -> None:
        for bad in (["a,b"], {"k": "v"}, True, 3, None):
            with self.subTest(bad=bad):
                self.load_expect_error(
                    self.config_spec(bad, split_csv=True),
                    containing="config[0].value",
                )

    def test_split_csv_must_be_a_boolean(self) -> None:
        for bad in ("yes", 1, None):
            with self.subTest(bad=bad):
                self.load_expect_error(
                    self.config_spec("a,b", split_csv=bad),
                    containing="config[0].split_csv",
                )

    def test_split_csv_applies_to_data_tokens(self) -> None:
        spec = self.load(self.config_spec("{data}/a, {data}/b", split_csv=True))
        home = str(Path.home() / ".openclaw")
        self.assertEqual(json.dumps([f"{home}/a", f"{home}/b"]), spec.config_entries[0].cli_value)


class IfEnvGuards(SpecTestCase):
    def test_guard_satisfied_when_all_vars_present(self) -> None:
        spec = self.load(self.config_spec("v", if_env=["A", "B"]))
        self.assertTrue(spec.config_entries[0].env_guard_satisfied({"A": "1", "B": "2"}))

    def test_guard_unsatisfied_when_any_var_missing(self) -> None:
        spec = self.load(self.config_spec("v", if_env=["A", "B"]))
        entry = spec.config_entries[0]
        self.assertFalse(entry.env_guard_satisfied({"A": "1"}))
        self.assertFalse(entry.env_guard_satisfied({}))

    def test_empty_guard_is_always_satisfied(self) -> None:
        spec = self.load(self.config_spec("v"))
        self.assertTrue(spec.config_entries[0].env_guard_satisfied({}))


class McpServerValidation(SpecTestCase):
    def test_command_and_url_together_rejected(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [{"name": "x", "command": "c", "url": "https://x"}]
        self.load_expect_error(spec, containing="mcp_servers[0]:")

    def test_neither_command_nor_url_rejected(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [{"name": "x", "no_probe": False}]
        self.load_expect_error(spec, containing="mcp_servers[0]:")

    def test_env_values_must_be_strings(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [{"name": "x", "command": "c", "env": {"K": 1}}]
        self.load_expect_error(spec, containing="mcp_servers[0].env.K")

    def test_timeout_must_be_an_integer(self) -> None:
        for bad in ("30", True, 1.5):
            with self.subTest(bad=bad):
                spec = copy.deepcopy(MINIMAL)
                spec["mcp_servers"] = [{"name": "x", "command": "c", "timeout": bad}]
                self.load_expect_error(spec, containing="mcp_servers[0].timeout")

    def test_remote_timeout_must_be_an_integer(self) -> None:
        for bad in ("30", True, 1.5):
            with self.subTest(bad=bad):
                spec = copy.deepcopy(MINIMAL)
                spec["mcp_servers"] = [{"name": "x", "url": "https://x", "timeout": bad}]
                self.load_expect_error(spec, containing="mcp_servers[0].timeout")

    def test_remote_timeout_accepted(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [{"name": "x", "url": "https://x", "timeout": 9}]
        server = self.load(spec).mcp_servers[0]
        self.assertIsInstance(server, RemoteMcpServer)
        self.assertEqual(9, server.timeout)

    def test_templating_applies_to_local_server_strings(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [
            {
                "name": "local",
                "command": "{data}/bin/tool",
                "args": ["--host", "{env:GREETING}"],
                "env": {"SECRET": "{env:SECRET}"},
            }
        ]
        server = self.load(spec).mcp_servers[0]
        home = str(Path.home() / ".openclaw")
        self.assertEqual(f"{home}/bin/tool", server.command)
        self.assertEqual(("--host", "hello"), server.args)
        self.assertEqual({"SECRET": "s3cret"}, server.env)

    def test_templating_applies_to_remote_url_and_headers(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = [
            {
                "name": "remote",
                "url": "https://api.example.com/mcp?key={env:SECRET}",
                "headers": {"Authorization": "Bearer {env:SECRET}"},
            }
        ]
        server = self.load(spec).mcp_servers[0]
        self.assertEqual("https://api.example.com/mcp?key=s3cret", server.url)
        self.assertEqual({"Authorization": "Bearer s3cret"}, server.headers)


class McpCliArgs(unittest.TestCase):
    def test_local_args_exact_with_all_options(self) -> None:
        server = LocalMcpServer(
            name="fs",
            command="node",
            args=("tool.js", "--verbose"),
            env={"A": "1", "B": "2"},
            no_probe=True,
            timeout=30,
        )
        self.assertEqual(
            [
                "--command",
                "node",
                "--arg",
                "tool.js",
                "--arg",
                "--verbose",
                "--env",
                "A=1",
                "--env",
                "B=2",
                "--no-probe",
                "--timeout",
                "30",
            ],
            mcp_to_cli_args(server),
        )

    def test_local_defaults_emit_command_and_no_probe_only(self) -> None:
        server = LocalMcpServer(name="fs", command="run")
        self.assertEqual(["--command", "run", "--no-probe"], mcp_to_cli_args(server))

    def test_remote_args_exact_with_header(self) -> None:
        # Verified against the real CLI at the pinned base image tag
        # (2026.7.1-2): no --type option exists, --header takes KEY=VALUE.
        server = RemoteMcpServer(
            name="sentiment",
            url="https://mcp.example.com/s",
            headers={"Authorization": "Bearer sk-x"},
        )
        self.assertEqual(
            [
                "--url",
                "https://mcp.example.com/s",
                "--header",
                "Authorization=Bearer sk-x",
                "--no-probe",
            ],
            mcp_to_cli_args(server),
        )

    def test_remote_defaults_emit_url_no_probe_only(self) -> None:
        server = RemoteMcpServer(name="bare", url="https://mcp.example.com")
        self.assertEqual(
            ["--url", "https://mcp.example.com", "--no-probe"],
            mcp_to_cli_args(server),
        )

    def test_remote_timeout_is_emitted_in_seconds(self) -> None:
        server = RemoteMcpServer(name="slow", url="https://mcp.example.com", timeout=11)
        self.assertEqual(
            ["--url", "https://mcp.example.com", "--no-probe", "--timeout", "11"],
            mcp_to_cli_args(server),
        )


class PluginValidation(SpecTestCase):
    def test_registry_plugin_ok(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["plugins"] = [{"name": "approvals"}]
        loaded = self.load(spec)
        self.assertEqual([Plugin(name="approvals", source=None)], loaded.plugins)

    def test_local_plugin_with_absolute_path_ok(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["plugins"] = [{"name": "tools", "source": "/opt/agent/plugins/tools"}]
        loaded = self.load(spec)
        self.assertEqual("/opt/agent/plugins/tools", loaded.plugins[0].source)

    def test_local_plugin_with_relative_path_rejected(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["plugins"] = [{"name": "tools", "source": "opt/agent/plugins/tools"}]
        self.load_expect_error(spec, containing="plugins[0].source")


class ChannelsDefaults(SpecTestCase):
    def test_use_env_defaults_to_true(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["channels"] = [{"type": "telegram"}]
        channel = self.load(spec).channels[0]
        self.assertEqual("telegram", channel.type)
        self.assertTrue(channel.use_env)

    def test_use_env_false_is_honored(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["channels"] = [{"type": "telegram", "use_env": False}]
        self.assertFalse(self.load(spec).channels[0].use_env)

    def test_channel_type_must_be_a_string(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["channels"] = [{"type": 5}]
        self.load_expect_error(spec, containing="channels[0].type")


class LoaderEdges(SpecTestCase):
    def test_invalid_json_is_rejected(self) -> None:
        path = self.tmp / "spec.json"
        path.write_text("{not json", encoding="utf-8")
        with self.assertRaises(SpecError) as ctx:
            load_spec(path, BASE_ENV)
        self.assertIn("not valid JSON", str(ctx.exception))

    def test_root_must_be_an_object(self) -> None:
        with self.assertRaises(SpecError) as ctx:
            self.load([1, 2])
        self.assertIn("$", str(ctx.exception))

    def test_optional_sections_default_to_empty(self) -> None:
        spec = self.load(MINIMAL)
        self.assertEqual([], spec.config_entries)
        self.assertEqual([], spec.channels)
        self.assertEqual([], spec.mcp_servers)
        self.assertEqual([], spec.plugins)
        self.assertEqual(Features(gh_auth=False), spec.features)

    def test_config_must_be_a_list(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["config"] = {"path": "a.b"}
        self.load_expect_error(spec, containing="config:")

    def test_mcp_servers_must_be_a_list(self) -> None:
        spec = copy.deepcopy(MINIMAL)
        spec["mcp_servers"] = {"name": "x"}
        self.load_expect_error(spec, containing="mcp_servers:")


if __name__ == "__main__":
    unittest.main()
