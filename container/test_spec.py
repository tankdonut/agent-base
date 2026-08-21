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
}

GOLDEN_ENV: Mapping[str, str] = {
    "TELEGRAM_BOT_TOKEN": "tg-token-123",
    "OPENCLAW_GATEWAY_TOKEN": "gw-token-456",
    "AC_INFINITY_EMAIL": "grower@example.com",
    "AC_INFINITY_PASSWORD": "s3cret",
    "SENTIMENT_API_KEY": "sk-sentiment",
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
        self.assertEqual(Features(gh_auth=True), spec.features)

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
        spec = self.load(self.config_spec("{env:TEMPLATE_VAR}"), env={"TEMPLATE_VAR": "{data}"})
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
    CSV_ENV: Mapping[str, str] = {"CSV_LIST": " 111 , 222 ,, 333 "}

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
        server = RemoteMcpServer(
            name="sentiment",
            url="https://mcp.example.com/s",
            headers={"Authorization": "Bearer sk-x"},
        )
        self.assertEqual(
            [
                "--type",
                "remote",
                "--url",
                "https://mcp.example.com/s",
                "--header",
                "Authorization: Bearer sk-x",
                "--no-probe",
            ],
            mcp_to_cli_args(server),
        )

    def test_remote_defaults_emit_type_url_no_probe_only(self) -> None:
        server = RemoteMcpServer(name="bare", url="https://mcp.example.com")
        self.assertEqual(
            ["--type", "remote", "--url", "https://mcp.example.com", "--no-probe"],
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
