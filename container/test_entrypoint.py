#!/usr/bin/env python3
"""Self-contained unittest suite for container/entrypoint.py.

Ports the freya/mimir config fast-path matrices to the generic module and
locks the spec-driven behaviour: first-boot sequence, config/mcp/plugin
reconciliation, gh auth, content-seeding semantics, the post-startup memory
ladder (force/incremental/skip, degraded detection, retry), fork/execvp
handoff, --validate-spec mode, fixture-project boots, and the secrets
canary (resolved values never reach logs).

Runs directly with no pytest dependency:

    python3 -m unittest discover -s container -p "test_entrypoint.py" -v

Importing entrypoint must not trigger boot side effects: main() is guarded
by ``if __name__ == "__main__":`` and the only import-time statement is an
os.environ.pop of OPENCLAW_HOME (inert in a test process).

Every test runs HOME-isolated in a TemporaryDirectory ({data} is
Path.home()/".openclaw") with AGENT_SPEC_PATH/AGENT_AUTOMATIONS_DIR pointed
into it, SEED_BASE patched, seed_automations.main mocked, and subprocess.run
captured — no real openclaw is ever spawned.
"""

from __future__ import annotations

import ast
import copy
import io
import json
import os
import subprocess
import tempfile
import time
import unittest
from collections.abc import Callable
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import entrypoint
from spec import SPEC_VERSION_SUPPORTED

REPO_ROOT = Path(__file__).resolve().parent.parent
FIXTURES = REPO_ROOT / "fixtures"

MINIMAL_SPEC: dict[str, object] = {
    "specVersion": 1,
    "agent": {"name": "t-agent"},
    "setup": {"auth_choice": "zai-coding-global"},
    "model": {"fallback": "zai/glm-4.7"},
    "automations": {"model": "zai/glm-4.7"},
    "config": [
        {"path": "channels.telegram.dmPolicy", "value": "allowlist"},
        # Guarded literal: exercises if_env skip/apply without making the
        # spec unloadable when the var is absent (templates resolve at load
        # time regardless of guards — see the templated variant below).
        {
            "path": "agents.defaults.heartbeat.target",
            "value": "telegram",
            "if_env": ["TELEGRAM_CHAT_ID"],
        },
    ],
}

FREYA_ENV = {
    "TELEGRAM_ALLOWED_USERS": "111, 222",
    "TELEGRAM_CHAT_ID": "-100123",
    "AC_INFINITY_EMAIL": "grower@example.com",
    "AC_INFINITY_PASSWORD": "ac-secret",
    "AGENT_GIT_TOKEN": "ghp-freya-token",
    "ZAI_API_KEY": "zai-key",
}

MIMIR_ENV = {
    "TELEGRAM_CHAT_ID": "-100999",
    "ALPHAVANTAGE_API_KEY": "av-key",
    "LUNARCRUSH_API_KEY": "lc-key",
    "DATABASE_URL": "postgres://db/trade",
    "ZAI_API_KEY": "zai-key",
}


class ChildExited(Exception):
    """Raised by the fake os._exit so the child branch stops like the real one."""


class ModuleImportSafety(unittest.TestCase):
    def test_main_guarded_by_name_main(self) -> None:
        source = Path(entrypoint.__file__).read_text(encoding="utf-8")
        self.assertIn('if __name__ == "__main__":', source)

    def test_import_exposes_main_without_booting(self) -> None:
        # Reaching this assertion proves the module import above did not
        # exec the gateway (os.execvp would have replaced the process).
        self.assertTrue(callable(entrypoint.main))

    def test_openclaw_home_popped_at_import(self) -> None:
        import importlib

        with mock.patch.dict(os.environ, {"OPENCLAW_HOME": "/tmp/x"}):
            importlib.reload(entrypoint)
            self.assertNotIn("OPENCLAW_HOME", os.environ)

    def test_only_standard_agent_env_names_are_consulted(self) -> None:
        tree = ast.parse(Path(entrypoint.__file__).read_text(encoding="utf-8"))
        consulted: list[str] = []
        for node in ast.walk(tree):
            if (
                isinstance(node, ast.Call)
                and isinstance(node.func, ast.Attribute)
                and node.func.attr == "get"
                and node.args
                and isinstance(node.args[0], ast.Constant)
                and isinstance(node.args[0].value, str)
            ):
                consulted.append(node.args[0].value)
        for legacy in consulted:
            self.assertFalse(
                legacy.startswith(("FREYA_", "MIMIR_", "TELEGRAM_")),
                f"legacy env name consulted: {legacy}",
            )
        for standard in (
            "AGENT_SPEC_PATH",
            "AGENT_MANAGE_CONFIG",
            "AGENT_SKIP_SEED",
            "AGENT_MEMORY_REINDEX",
            "AGENT_GIT_TOKEN",
        ):
            self.assertIn(standard, consulted)


class EntrypointTestCase(unittest.TestCase):
    """HOME-isolated harness: spec + automations + seeds in a tmpdir,
    subprocess.run captured, seed_automations.main mocked."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = Path(tmp.name)
        self.data = self.home / ".openclaw"
        self.seed_base = self.home / "seed-base"
        self.automations = self.home / "automations"
        self.seed_base.mkdir()
        self._write_default_automation(self.automations)

        self.calls: list[list[str]] = []
        self.stdin_inputs: list[str] = []
        self.automation_argv: list[list[str]] = []
        self.handler: Callable[[list[str]], subprocess.CompletedProcess[str]] = self._ok

        env = {
            "HOME": str(self.home),
            "PATH": os.environ.get("PATH", ""),
            "AGENT_SPEC_PATH": str(self._write_spec(MINIMAL_SPEC)),
            "AGENT_AUTOMATIONS_DIR": str(self.automations),
            "ZAI_API_KEY": "zai-key",
        }
        env_patcher = mock.patch.dict(os.environ, env, clear=True)
        env_patcher.start()
        self.addCleanup(env_patcher.stop)

        seed_patcher = mock.patch.object(entrypoint, "SEED_BASE", self.seed_base)
        seed_patcher.start()
        self.addCleanup(seed_patcher.stop)

        run_patcher = mock.patch("subprocess.run", self._capture_run)
        run_patcher.start()
        self.addCleanup(run_patcher.stop)

        automation_patcher = mock.patch.object(
            entrypoint.seed_automations,
            "main",
            side_effect=lambda argv: self.automation_argv.append(list(argv)),
        )
        automation_patcher.start()
        self.addCleanup(automation_patcher.stop)

        entrypoint.config_reconcile_stats.update({"applied": 0, "skipped": 0})

    @staticmethod
    def _ok(cmd: list[str]) -> subprocess.CompletedProcess[str]:
        stdout = '{"findings":[]}' if cmd[:2] == ["openclaw", "doctor"] else ""
        return subprocess.CompletedProcess(cmd, 0, stdout=stdout, stderr="")

    def _write_doctor_marker(self, content: str) -> None:
        self.data.mkdir(parents=True, exist_ok=True)
        (self.data / "doctor-disabled-skills").write_text(content, encoding="utf-8")

    def _capture_run(
        self, cmd: list[str] | tuple[str, ...], **kwargs: object
    ) -> subprocess.CompletedProcess[str]:
        cmd_list = list(cmd)
        self.calls.append(cmd_list)
        input_data = kwargs.get("input")
        if isinstance(input_data, str):
            self.stdin_inputs.append(input_data)
        return self.handler(cmd_list)

    def _write_spec(self, spec: object) -> Path:
        path = self.home / "spec.json"
        path.write_text(json.dumps(spec), encoding="utf-8")
        return path

    @staticmethod
    def _write_default_automation(directory: Path) -> None:
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "heartbeat.md").write_text(
            "---\nname: heartbeat\nevery: 15m\ndeliver: no-deliver\n---\nPing.\n",
            encoding="utf-8",
        )

    # --- assertion helpers ---

    def calls_with(self, *prefix: str) -> list[list[str]]:
        wanted = list(prefix)
        return [c for c in self.calls if c[: len(wanted)] == wanted]

    def has_call(self, *prefix: str) -> bool:
        return bool(self.calls_with(*prefix))

    def index_of(self, *prefix: str) -> int:
        wanted = list(prefix)
        for i, call in enumerate(self.calls):
            if call[: len(wanted)] == wanted:
                return i
        raise AssertionError(f"no call with prefix {wanted} in {self.calls}")

    def write_openclaw_config(self, payload: str) -> None:
        self.data.mkdir(parents=True, exist_ok=True)
        (self.data / "openclaw.json").write_text(payload, encoding="utf-8")

    def capture(self, func: Callable[[], object]) -> tuple[str, str]:
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            func()
        return out.getvalue(), err.getvalue()

    def load_default_spec(self) -> entrypoint.Spec:
        return entrypoint.load_agent_spec(os.environ)

    def load_spec_with(
        self, spec: object, env_extra: dict[str, str] | None = None
    ) -> entrypoint.Spec:
        path = self._write_spec(spec)
        env = dict(os.environ)
        env["AGENT_SPEC_PATH"] = str(path)
        env.update(env_extra or {})
        return entrypoint.load_agent_spec(env)

    def boot(self, argv: list[str] | None = None) -> SimpleNamespace:
        """Run main() on the parent path (fork -> child pid) with execvp captured."""
        argv = ["openclaw", "gateway"] if argv is None else argv
        out, err = io.StringIO(), io.StringIO()
        with (
            mock.patch.object(os, "fork", return_value=1234) as fork_mock,
            mock.patch.object(os, "execvp") as execvp_mock,
            redirect_stdout(out),
            redirect_stderr(err),
        ):
            code = entrypoint.main(argv)
        return SimpleNamespace(
            code=code,
            stdout=out.getvalue(),
            stderr=err.getvalue(),
            fork=fork_mock,
            execvp=execvp_mock,
        )

    def boot_child(self, argv: list[str] | None = None) -> SimpleNamespace:
        """Run main() on the child path (fork -> 0); os._exit raises ChildExited."""
        argv = ["openclaw", "gateway"] if argv is None else argv
        exit_codes: list[int] = []

        def fake_exit(code: int) -> None:
            exit_codes.append(code)
            raise ChildExited

        out, err = io.StringIO(), io.StringIO()
        with (
            mock.patch.object(os, "fork", return_value=0),
            mock.patch.object(os, "_exit", side_effect=fake_exit),
            mock.patch.object(os, "execvp") as execvp_mock,
            redirect_stdout(out),
            redirect_stderr(err),
            self.assertRaises(ChildExited),
        ):
            entrypoint.main(argv)
        return SimpleNamespace(
            stdout=out.getvalue(),
            stderr=err.getvalue(),
            exit_codes=exit_codes,
            execvp=execvp_mock,
        )


class ConfigSetSkipMatrix(EntrypointTestCase):
    """Ported from freya/mimir: config_set only shells out when the stored
    value actually differs; missing/malformed/ambiguous shapes hit the CLI."""

    def call(self, key: str, value: str, *extra: str) -> tuple[str, str]:
        return self.capture(lambda: entrypoint.config_set(key, value, *extra))

    def assert_skipped(self) -> None:
        self.assertEqual([], self.calls_with("openclaw", "config", "set"))
        self.assertEqual({"applied": 0, "skipped": 1}, entrypoint.config_reconcile_stats)

    def assert_applied(self, *expected: str) -> None:
        self.assertEqual(
            [["openclaw", "config", "set", *expected]],
            self.calls_with("openclaw", "config", "set"),
        )
        self.assertEqual({"applied": 1, "skipped": 0}, entrypoint.config_reconcile_stats)

    def test_matching_string_value_skips_cli(self) -> None:
        self.write_openclaw_config('{"channels": {"telegram": {"dmPolicy": "allowlist"}}}')
        self.call("channels.telegram.dmPolicy", "allowlist")
        self.assert_skipped()

    def test_differing_string_value_calls_cli(self) -> None:
        self.write_openclaw_config('{"channels": {"telegram": {"dmPolicy": "allowlist"}}}')
        self.call("channels.telegram.dmPolicy", "denyall")
        self.assert_applied("channels.telegram.dmPolicy", "denyall")

    def test_missing_key_calls_cli(self) -> None:
        self.write_openclaw_config('{"channels": {}}')
        self.call("channels.telegram.dmPolicy", "allowlist")
        self.assert_applied("channels.telegram.dmPolicy", "allowlist")

    def test_matching_strict_json_structure_skips_cli(self) -> None:
        targets = [{"channel": "telegram", "to": "-100200", "threadId": "7"}]
        self.write_openclaw_config(json.dumps({"approvals": {"plugin": {"targets": targets}}}))
        self.call("approvals.plugin.targets", json.dumps(targets), "--strict-json")
        self.assert_skipped()

    def test_strict_json_string_value_skips_cli(self) -> None:
        self.write_openclaw_config('{"agents": {"defaults": {"heartbeat": {"to": "-100200"}}}}')
        self.call("agents.defaults.heartbeat.to", '"-100200"', "--strict-json")
        self.assert_skipped()

    def test_coerced_bool_storage_skips_cli(self) -> None:
        # The CLI stores bare "true" as boolean true — that still matches.
        self.write_openclaw_config('{"audit": {"enabled": true}}')
        self.call("audit.enabled", "true")
        self.assert_skipped()

    def test_coerced_number_storage_skips_cli(self) -> None:
        self.write_openclaw_config('{"channels": {"telegram": {"mediaMaxMb": 20}}}')
        self.call("channels.telegram.mediaMaxMb", "20")
        self.assert_skipped()

    def test_number_one_is_not_boolean_true(self) -> None:
        # True == 1 in Python; the bool guard must treat it as drift.
        self.write_openclaw_config('{"audit": {"enabled": 1}}')
        self.call("audit.enabled", "true")
        self.assert_applied("audit.enabled", "true")

    def test_missing_config_file_calls_cli(self) -> None:
        self.assertFalse((self.data / "openclaw.json").exists())
        self.call("audit.enabled", "true")
        self.assert_applied("audit.enabled", "true")

    def test_malformed_config_file_calls_cli(self) -> None:
        self.write_openclaw_config('{"audit": "not closed')
        self.call("audit.enabled", "true")
        self.assert_applied("audit.enabled", "true")

    def test_unparseable_strict_json_calls_cli(self) -> None:
        self.write_openclaw_config('{"audit": {"enabled": true}}')
        self.call("audit.enabled", "{oops", "--strict-json")
        self.assert_applied("audit.enabled", "{oops", "--strict-json")

    def test_failed_set_warns_with_key_but_never_value(self) -> None:
        def failing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="")

        self.handler = failing
        _out, err = self.call("audit.enabled", "true")
        self.assertIn("config set failed: audit.enabled", err)
        self.assertNotIn("true", err.split("config set failed: audit.enabled", 1)[1])

    def test_non_dict_config_calls_cli(self) -> None:
        self.write_openclaw_config('["not", "a", "dict"]')
        self.call("audit.enabled", "true")
        self.assert_applied("audit.enabled", "true")

    def test_nested_path_through_non_dict_segment(self) -> None:
        self.write_openclaw_config('{"agents": {"defaults": "flat"}}')
        self.call("agents.defaults.utilityModel", "zai/glm-4.7")
        self.assert_applied("agents.defaults.utilityModel", "zai/glm-4.7")


class FirstBootSequence(EntrypointTestCase):
    SETUP_CMD = [
        "openclaw",
        "setup",
        "--non-interactive",
        "--accept-risk",
        "--auth-choice",
        "zai-coding-global",
        "--skip-channels",
        "--skip-skills",
        "--skip-daemon",
        "--skip-ui",
        "--skip-health",
        "--skip-search",
    ]

    def test_first_boot_runs_setup_fallback_channels_llama_in_order(self) -> None:
        spec = copy.deepcopy(MINIMAL_SPEC)
        spec["channels"] = [{"type": "telegram"}, {"type": "discord", "use_env": False}]
        self._write_spec(spec)
        result = self.boot()
        self.assertEqual(0, result.code)
        self.assertEqual(self.SETUP_CMD, self.calls[0])
        self.assertEqual(["openclaw", "models", "fallbacks", "add", "zai/glm-4.7"], self.calls[1])
        self.assertEqual(
            ["openclaw", "channels", "add", "--channel", "telegram", "--use-env"], self.calls[2]
        )
        self.assertEqual(["openclaw", "channels", "add", "--channel", "discord"], self.calls[3])
        self.assertEqual(
            ["openclaw", "plugins", "install", "@openclaw/llama-cpp-provider"], self.calls[4]
        )

    def test_auth_choice_and_fallback_come_from_spec(self) -> None:
        spec = copy.deepcopy(MINIMAL_SPEC)
        spec["setup"] = {"auth_choice": "zai-coding-cn"}
        spec["model"] = {"fallback": "zai/glm-4.6"}
        self._write_spec(spec)
        self.boot()
        self.assertIn("--auth-choice", self.calls[0])
        self.assertIn("zai-coding-cn", self.calls[0])
        self.assertEqual(["openclaw", "models", "fallbacks", "add", "zai/glm-4.6"], self.calls[1])

    def test_warm_boot_skips_first_boot_entirely(self) -> None:
        self.write_openclaw_config("{}")
        self.boot()
        self.assertFalse(self.has_call("openclaw", "setup"))
        self.assertFalse(self.has_call("openclaw", "models", "fallbacks"))
        self.assertFalse(self.has_call("openclaw", "channels", "add"))
        self.assertFalse(self.has_call("openclaw", "plugins", "install", "@openclaw/llama"))


class FirstBootEnvGate(EntrypointTestCase):
    """A failed first-boot setup must abort cleanly (named env var, exit 1,
    no traceback) instead of crash-looping on an unhandled exception."""

    def _zai_spec(self) -> entrypoint.Spec:
        return entrypoint.Spec(
            agent_name="t-agent",
            auth_choice="zai-coding-global",
            model_fallback="zai/glm-4.7",
            automations_model="zai/glm-4.7",
        )

    def test_setup_failure_aborts_cleanly_naming_env_var(self) -> None:
        def failing_setup(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "setup"]:
                raise subprocess.CalledProcessError(1, cmd)
            return self._ok(cmd)

        self.handler = failing_setup
        err = io.StringIO()
        with self.assertRaises(SystemExit) as ctx, redirect_stderr(err):
            entrypoint.first_boot_setup(self._zai_spec())
        self.assertEqual(1, ctx.exception.code)
        self.assertIn("first-boot setup failed (exit 1)", err.getvalue())
        self.assertIn("ZAI_API_KEY", err.getvalue())
        # The value never appears — only the var name (secrets discipline).
        self.assertNotIn("zai-key", err.getvalue())

    def test_setup_failure_for_non_zai_auth_aborts_without_env_hint(self) -> None:
        def failing_setup(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "setup"]:
                raise subprocess.CalledProcessError(2, cmd)
            return self._ok(cmd)

        self.handler = failing_setup
        spec = entrypoint.Spec(
            agent_name="t-agent",
            auth_choice="manual",
            model_fallback="zai/glm-4.7",
            automations_model="zai/glm-4.7",
        )
        err = io.StringIO()
        with self.assertRaises(SystemExit) as ctx, redirect_stderr(err):
            entrypoint.first_boot_setup(spec)
        self.assertEqual(1, ctx.exception.code)
        self.assertIn("first-boot setup failed (exit 2)", err.getvalue())
        self.assertNotIn("ZAI_API_KEY", err.getvalue())

    def test_boot_without_zai_api_key_aborts_at_load(self) -> None:
        with mock.patch.dict(os.environ):
            os.environ.pop("ZAI_API_KEY", None)
            with self.assertRaises(entrypoint.SpecError) as ctx:
                self.boot()
        self.assertIn("ZAI_API_KEY", str(ctx.exception))
        self.assertIn("setup.auth_choice", str(ctx.exception))


class ReconcileConfigPhases(EntrypointTestCase):
    def test_guarded_entry_skipped_with_log_when_env_missing(self) -> None:
        spec = self.load_default_spec()
        self.assertNotIn("TELEGRAM_CHAT_ID", os.environ)
        out, err = self.capture(lambda: entrypoint.reconcile_config(spec, os.environ))
        self.assertEqual("", err)
        self.assertIn("skipped (if_env unsatisfied: agents.defaults.heartbeat.target)", out)
        self.assertEqual(
            [
                ["openclaw", "config", "set", "channels.telegram.dmPolicy", "allowlist"],
                *self._deny_calls(),
            ],
            self.calls_with("openclaw", "config", "set"),
        )

    def test_guarded_entry_applied_when_env_present(self) -> None:
        spec_doc = copy.deepcopy(MINIMAL_SPEC)
        spec_doc["config"] = [
            {"path": "channels.telegram.dmPolicy", "value": "allowlist"},
            {
                "path": "agents.defaults.heartbeat.to",
                "value": "{env:TELEGRAM_CHAT_ID}",
                "strict": True,
                "if_env": ["TELEGRAM_CHAT_ID"],
            },
        ]
        env = dict(os.environ)
        env["TELEGRAM_CHAT_ID"] = "-100123"
        spec = self.load_spec_with(spec_doc, {"TELEGRAM_CHAT_ID": "-100123"})
        out, _err = self.capture(lambda: entrypoint.reconcile_config(spec, env))
        self.assertNotIn("if_env unsatisfied", out)
        self.assertEqual(
            [
                ["openclaw", "config", "set", "channels.telegram.dmPolicy", "allowlist"],
                [
                    "openclaw",
                    "config",
                    "set",
                    "agents.defaults.heartbeat.to",
                    '"-100123"',
                    "--strict-json",
                ],
                *self._deny_calls(),
            ],
            self.calls_with("openclaw", "config", "set"),
        )

    @staticmethod
    def _deny_calls() -> list[list[str]]:
        return [
            [
                "openclaw",
                "config",
                "set",
                "tools.deny",
                json.dumps(list(entrypoint.TOOLS_DENY_DEFAULT)),
                "--strict-json",
            ]
        ]

    def test_entries_applied_in_spec_order(self) -> None:
        spec_doc = copy.deepcopy(MINIMAL_SPEC)
        spec_doc["config"] = [
            {"path": "z.last", "value": "1"},
            {"path": "a.first", "value": "2"},
            {"path": "m.middle", "value": "3"},
        ]
        spec = self.load_spec_with(spec_doc)
        self.capture(lambda: entrypoint.reconcile_config(spec, os.environ))
        paths = [c[3] for c in self.calls_with("openclaw", "config", "set")]
        self.assertEqual(["z.last", "a.first", "m.middle", "tools.deny"], paths)

    def test_split_csv_entry_marshals_as_strict_json_list(self) -> None:
        spec_doc = copy.deepcopy(MINIMAL_SPEC)
        spec_doc["config"] = [
            {
                "path": "channels.telegram.allowFrom",
                "value": "{env:TELEGRAM_ALLOWED_USERS}",
                "split_csv": True,
            }
        ]
        spec = self.load_spec_with(spec_doc, {"TELEGRAM_ALLOWED_USERS": " 111 , 222 ,, "})
        self.capture(lambda: entrypoint.reconcile_config(spec, os.environ))
        self.assertEqual(
            [
                [
                    "openclaw",
                    "config",
                    "set",
                    "channels.telegram.allowFrom",
                    '["111", "222"]',
                    "--strict-json",
                ],
                *self._deny_calls(),
            ],
            self.calls_with("openclaw", "config", "set"),
        )

    def test_warm_volume_counts_skips_and_applies(self) -> None:
        self.write_openclaw_config('{"channels": {"telegram": {"dmPolicy": "allowlist"}}}')
        spec = self.load_default_spec()
        out, _err = self.capture(lambda: entrypoint.reconcile_config(spec, os.environ))
        # dmPolicy matched on disk (value-skip); the guarded heartbeat entry
        # is guard-skipped, which does not count as a value skip.
        self.assertIn("Config reconcile: 0 set, 1 already current", out)


class ReconcileMcpMatrix(EntrypointTestCase):
    LOCAL_SPEC: dict[str, object] = {
        **copy.deepcopy(MINIMAL_SPEC),
        "mcp_servers": [
            {"name": "acme", "command": "tool", "env": {"K": "V"}, "timeout": 60},
            {"name": "guarded", "url": "https://mcp.example.com/g", "if_env": ["NEEDED_KEY"]},
        ],
    }

    def test_missing_server_registered_with_builder_flags(self) -> None:
        spec = self.load_spec_with(self.LOCAL_SPEC)
        out, err = self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertEqual("", err)
        self.assertIn("Registering MCP server 'acme'", out)
        self.assertEqual(
            [
                "openclaw",
                "mcp",
                "add",
                "acme",
                "--command",
                "tool",
                "--env",
                "K=V",
                "--no-probe",
                "--timeout",
                "60",
            ],
            self.calls_with("openclaw", "mcp", "add")[0],
        )

    def test_existing_server_skipped(self) -> None:
        def listing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "mcp", "list"]:
                return subprocess.CompletedProcess(cmd, 0, stdout='{"acme": {}}', stderr="")
            return self._ok(cmd)

        self.handler = listing
        spec = self.load_spec_with(self.LOCAL_SPEC)
        out, _err = self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertIn("MCP server 'acme' already registered — skipping", out)
        self.assertEqual([], self.calls_with("openclaw", "mcp", "add"))

    def test_failed_listing_counts_as_missing(self) -> None:
        def failing_list(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "mcp", "list"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="boom")
            return self._ok(cmd)

        self.handler = failing_list
        spec = self.load_spec_with(self.LOCAL_SPEC)
        self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertTrue(self.has_call("openclaw", "mcp", "add", "acme"))

    def test_failed_add_warns_never_raises(self) -> None:
        def failing_add(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "mcp", "add"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="nope")
            return self._ok(cmd)

        self.handler = failing_add
        spec = self.load_spec_with(self.LOCAL_SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertIn("mcp add failed: acme", err)

    def test_guard_unsatisfied_skips_server(self) -> None:
        spec = self.load_spec_with(self.LOCAL_SPEC)  # NEEDED_KEY unset
        out, _err = self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertIn("MCP server 'guarded' skipped (if_env unsatisfied)", out)
        self.assertEqual([], self.calls_with("openclaw", "mcp", "add", "guarded"))

    def test_remote_server_flags_exact(self) -> None:
        spec_doc = copy.deepcopy(MINIMAL_SPEC)
        spec_doc["mcp_servers"] = [
            {
                "name": "sentiment",
                "url": "https://mcp.example.com/s?key={env:API_KEY}",
                "headers": {"Authorization": "Bearer {env:API_KEY}"},
            }
        ]
        spec = self.load_spec_with(spec_doc, {"API_KEY": "sk-1"})
        self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertEqual(
            [
                "openclaw",
                "mcp",
                "add",
                "sentiment",
                "--url",
                "https://mcp.example.com/s?key=sk-1",
                "--header",
                "Authorization=Bearer sk-1",
                "--no-probe",
            ],
            self.calls_with("openclaw", "mcp", "add")[0],
        )


class ReconcilePluginsMatrix(EntrypointTestCase):
    REGISTRY_SPEC: dict[str, object] = {
        **copy.deepcopy(MINIMAL_SPEC),
        "plugins": [{"name": "chart-renderer"}],
    }
    LOCAL_SPEC: dict[str, object] = {
        **copy.deepcopy(MINIMAL_SPEC),
        "plugins": [{"name": "approval-gate", "source": "/opt/seed/plugins/approval-gate"}],
    }

    def test_registry_plugin_installs_when_absent(self) -> None:
        spec = self.load_spec_with(self.REGISTRY_SPEC)
        out, _err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertIn("Installing plugin 'chart-renderer'", out)
        self.assertEqual(
            [["openclaw", "plugins", "install", "chart-renderer"]],
            self.calls_with("openclaw", "plugins", "install"),
        )

    def test_registry_plugin_skips_when_present(self) -> None:
        def listing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "plugins", "list"]:
                return subprocess.CompletedProcess(
                    cmd, 0, stdout='{"chart-renderer": {}}', stderr=""
                )
            return self._ok(cmd)

        self.handler = listing
        spec = self.load_spec_with(self.REGISTRY_SPEC)
        out, _err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertIn("Plugin 'chart-renderer' already installed — skipping", out)
        self.assertEqual([], self.calls_with("openclaw", "plugins", "install"))

    def test_registry_install_failure_warns(self) -> None:
        def failing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "plugins", "install"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="404")
            return self._ok(cmd)

        self.handler = failing
        spec = self.load_spec_with(self.REGISTRY_SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertIn("plugin install failed: chart-renderer", err)

    def test_local_plugin_force_installed_every_boot_without_listing(self) -> None:
        spec = self.load_spec_with(self.LOCAL_SPEC)
        self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertEqual(
            2,
            len(
                self.calls_with("openclaw", "plugins", "install", "/opt/seed/plugins/approval-gate")
            ),
        )
        for call in self.calls_with("openclaw", "plugins", "install"):
            self.assertIn("--force", call)
        # No first boot ran here → no agent-managed-plugins marker → the
        # orphan report is disabled and never lists.
        self.assertEqual([], self.calls_with("openclaw", "plugins", "list", "--json"))

    def test_local_plugin_install_failure_warns(self) -> None:
        def failing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "plugins", "install"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="boom")
            return self._ok(cmd)

        self.handler = failing
        spec = self.load_spec_with(self.LOCAL_SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertIn("plugin install failed: approval-gate", err)


class AuthenticateGhMatrix(EntrypointTestCase):
    def test_missing_token_warns_and_never_spawns(self) -> None:
        _out, err = self.capture(lambda: entrypoint.authenticate_gh({}))
        self.assertIn("AGENT_GIT_TOKEN not set — gh not authenticated", err)
        self.assertEqual([], self.calls)

    def test_missing_gh_binary_warns_and_never_spawns(self) -> None:
        with mock.patch.object(entrypoint.shutil, "which", return_value=None):
            _out, err = self.capture(lambda: entrypoint.authenticate_gh({"AGENT_GIT_TOKEN": "tok"}))
        self.assertIn("gh CLI not on PATH — skipping auth", err)
        self.assertEqual([], self.calls)

    def test_success_authenticates_via_stdin_token(self) -> None:
        with mock.patch.object(entrypoint.shutil, "which", return_value="/usr/bin/gh"):
            out, _err = self.capture(
                lambda: entrypoint.authenticate_gh({"AGENT_GIT_TOKEN": "tok-1"})
            )
        self.assertEqual([["gh", "auth", "login", "--with-token"]], self.calls)
        self.assertEqual(["tok-1"], self.stdin_inputs)
        self.assertIn("Authenticated gh CLI from AGENT_GIT_TOKEN", out)

    def test_failed_login_warns_without_token_in_logs(self) -> None:
        def failing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="bad credentials")

        self.handler = failing
        with mock.patch.object(entrypoint.shutil, "which", return_value="/usr/bin/gh"):
            _out, err = self.capture(
                lambda: entrypoint.authenticate_gh({"AGENT_GIT_TOKEN": "tok-secret"})
            )
        self.assertIn("gh auth login failed (exit 1) — gh not authenticated", err)
        self.assertIn("gh auth stderr: bad credentials", err)
        self.assertNotIn("tok-secret", err)

    def test_timeout_warns_and_is_non_fatal(self) -> None:
        def timing_out(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            raise subprocess.TimeoutExpired(cmd, 30)

        self.handler = timing_out
        with mock.patch.object(entrypoint.shutil, "which", return_value="/usr/bin/gh"):
            _out, err = self.capture(lambda: entrypoint.authenticate_gh({"AGENT_GIT_TOKEN": "tok"}))
        self.assertIn("gh auth login timed out — gh not authenticated", err)


class SeedContentSemantics(EntrypointTestCase):
    def make_seeds(self) -> None:
        (self.seed_base / "workspace" / "AGENTS.md").parent.mkdir(parents=True)
        (self.seed_base / "workspace" / "AGENTS.md").write_text("# seed\n", encoding="utf-8")
        (self.seed_base / "skills" / "propose.md").parent.mkdir(parents=True)
        (self.seed_base / "skills" / "propose.md").write_text("skill\n", encoding="utf-8")
        (self.seed_base / "docs" / "kb.md").parent.mkdir(parents=True)
        (self.seed_base / "docs" / "kb.md").write_text("docs\n", encoding="utf-8")

    def seed(self, env: dict[str, str] | None = None) -> tuple[str, str]:
        spec = self.load_default_spec()
        return self.capture(lambda: entrypoint.seed_content(spec, env or dict(os.environ)))

    def test_first_boot_copies_workspace_skills_and_docs(self) -> None:
        self.make_seeds()
        self.seed()
        self.assertEqual("# seed\n", (self.data / "workspace" / "AGENTS.md").read_text("utf-8"))
        self.assertEqual("skill\n", (self.data / "skills" / "propose.md").read_text("utf-8"))
        self.assertEqual("docs\n", (self.data / "workspace" / "docs" / "kb.md").read_text("utf-8"))
        self.assertTrue((self.data / "workspace" / "journal").is_dir())
        # The hard standard: docs live under workspace/, never {data}/docs.
        self.assertFalse((self.data / "docs").exists())

    def test_workspace_is_first_boot_only_and_survives_evolution(self) -> None:
        self.make_seeds()
        self.seed()
        evolved = self.data / "workspace" / "AGENTS.md"
        evolved.write_text("# evolved by agent\n", encoding="utf-8")
        (self.data / "workspace" / "journal" / "entry.md").write_text("j\n", encoding="utf-8")
        self.seed()
        self.assertEqual("# evolved by agent\n", evolved.read_text("utf-8"))
        self.assertTrue((self.data / "workspace" / "journal" / "entry.md").is_file())

    def test_skills_replaced_every_boot(self) -> None:
        self.make_seeds()
        self.seed()
        stale = self.data / "skills" / "stale-skill.md"
        stale.write_text("old\n", encoding="utf-8")
        self.seed()
        self.assertFalse(stale.exists())
        self.assertTrue((self.data / "skills" / "propose.md").is_file())

    def test_docs_replaced_every_boot(self) -> None:
        self.make_seeds()
        self.seed()
        stale = self.data / "workspace" / "docs" / "stale.md"
        stale.write_text("old\n", encoding="utf-8")
        self.seed()
        self.assertFalse(stale.exists())
        self.assertTrue((self.data / "workspace" / "docs" / "kb.md").is_file())

    def test_no_seeds_still_creates_workspace_and_journal(self) -> None:
        out, err = self.seed()
        self.assertEqual("", err)
        self.assertTrue((self.data / "workspace" / "journal").is_dir())

    def test_skip_seed_env_skips_content_seeding_only(self) -> None:
        self.make_seeds()
        out, _err = self.seed({"AGENT_SKIP_SEED": "1"})
        self.assertIn("Content seeding skipped (AGENT_SKIP_SEED=1", out)
        self.assertFalse((self.data / "workspace").exists())
        self.assertFalse((self.data / "skills").exists())


class MemoryStatusLadder(EntrypointTestCase):
    def status(self, payload: str) -> str:
        def handler(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:4] == ["openclaw", "memory", "status", "--agent"]:
                return subprocess.CompletedProcess(cmd, 0, stdout=payload, stderr="")
            return self._ok(cmd)

        self.handler = handler
        action_holder: list[str] = []

        def run_it() -> None:
            action_holder.append(entrypoint.check_memory_status())

        self.capture(run_it)
        return action_holder[0]

    def test_clean_index_skips(self) -> None:
        action = self.status('[{"status": {"files": 5, "dirty": false}}]')
        self.assertEqual("skip", action)

    def test_dirty_index_is_incremental(self) -> None:
        action = self.status('[{"status": {"files": 5, "dirty": true}}]')
        self.assertEqual("incremental", action)

    def test_identity_mismatched_is_force(self) -> None:
        action = self.status(
            '[{"status": {"files": 5, "custom": {"indexIdentity": '
            '{"status": "mismatched", "reason": "model changed"}}}}]'
        )
        self.assertEqual("force", action)

    def test_identity_missing_is_force(self) -> None:
        action = self.status(
            '[{"status": {"files": 5, "custom": {"indexIdentity": {"status": "missing"}}}}]'
        )
        self.assertEqual("force", action)

    def test_unparseable_json_defaults_to_force(self) -> None:
        self.assertEqual("force", self.status(""))

    def test_malformed_structure_defaults_to_force(self) -> None:
        self.assertEqual("force", self.status('[{"no_status": 1}]'))

    def test_nonzero_exit_defaults_to_force(self) -> None:
        def failing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            return subprocess.CompletedProcess(cmd, 2, stdout="[]", stderr="")

        self.handler = failing
        action_holder: list[str] = []
        self.capture(lambda: action_holder.append(entrypoint.check_memory_status()))
        self.assertEqual("force", action_holder[0])

    def test_timeout_defaults_to_force(self) -> None:
        def timing_out(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            raise subprocess.TimeoutExpired(cmd, 60)

        self.handler = timing_out
        action_holder: list[str] = []
        self.capture(lambda: action_holder.append(entrypoint.check_memory_status()))
        self.assertEqual("force", action_holder[0])


class ReindexRetryMatrix(EntrypointTestCase):
    def memory_index_calls(self) -> list[list[str]]:
        # The ported command inserts --force at position 2 (verbatim from
        # the freya/mimir originals): ["openclaw", "memory", "--force", "index", ...]
        return [c for c in self.calls if c[:2] == ["openclaw", "memory"] and "index" in c]

    def reindex(
        self, results: list[subprocess.CompletedProcess[str]], force: bool = True
    ) -> tuple[str, str]:
        queue = list(results)

        def handler(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "memory"] and "index" in cmd:
                if queue:
                    item = queue.pop(0)
                    return subprocess.CompletedProcess(
                        cmd, item.returncode, item.stdout, item.stderr
                    )
                return self._ok(cmd)
            return self._ok(cmd)

        self.handler = handler
        with mock.patch.object(time, "sleep"):
            return self.capture(lambda: entrypoint.reindex_memory(force=force))

    def test_success_on_first_attempt(self) -> None:
        out, err = self.reindex([subprocess.CompletedProcess([], 0, stdout="", stderr="")])
        self.assertEqual("", err)
        self.assertIn("memory reindex succeeded (attempt 1/3)", out)
        self.assertEqual(1, len(self.memory_index_calls()))

    def test_force_flag_inserted_for_full_rebuild(self) -> None:
        self.reindex([subprocess.CompletedProcess([], 0, stdout="", stderr="")], force=True)
        self.assertEqual(
            ["openclaw", "memory", "--force", "index", "--agent", "main", "--verbose"],
            self.memory_index_calls()[0],
        )

    def test_no_force_flag_for_incremental(self) -> None:
        self.reindex([subprocess.CompletedProcess([], 0, stdout="", stderr="")], force=False)
        self.assertEqual(
            ["openclaw", "memory", "index", "--agent", "main", "--verbose"],
            self.memory_index_calls()[0],
        )

    def test_failure_retries_three_times_then_gives_up(self) -> None:
        _out, err = self.reindex(
            [
                subprocess.CompletedProcess([], 1, stdout="", stderr="e1"),
                subprocess.CompletedProcess([], 1, stdout="", stderr="e2"),
                subprocess.CompletedProcess([], 1, stdout="", stderr="e3"),
            ]
        )
        self.assertEqual(3, len(self.memory_index_calls()))
        self.assertIn("memory reindex failed after 3 attempts", err)

    def test_degraded_success_is_detected_and_retried(self) -> None:
        degraded = subprocess.CompletedProcess([], 0, stdout="", stderr="chunks_vec not updated")
        _out, err = self.reindex([degraded, degraded, degraded])
        self.assertEqual(3, len(self.memory_index_calls()))
        self.assertIn("degraded — vectors skipped", err)
        self.assertIn("memory reindex failed after 3 attempts", err)

    def test_degraded_then_clean_succeeds(self) -> None:
        out, err = self.reindex(
            [
                subprocess.CompletedProcess([], 0, stdout="", stderr="chunks_vec not updated"),
                subprocess.CompletedProcess([], 0, stdout="", stderr=""),
            ]
        )
        self.assertIn("degraded — vectors skipped", err)
        self.assertIn("memory reindex succeeded (attempt 2/3)", out)
        self.assertNotIn("memory reindex failed after 3 attempts", err)

    def test_timeout_retries_then_gives_up(self) -> None:
        def timing_out(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            raise subprocess.TimeoutExpired(cmd, 600)

        self.handler = timing_out
        with mock.patch.object(time, "sleep"):
            _out, err = self.capture(lambda: entrypoint.reindex_memory())
        self.assertEqual(3, len(self.memory_index_calls()))
        self.assertIn("memory reindex failed after 3 attempts", err)


class PostStartupFlow(EntrypointTestCase):
    def run_post_startup(self, env: dict[str, str] | None = None) -> tuple[str, str]:
        spec = self.load_default_spec()
        return self.capture(lambda: entrypoint.post_startup(spec, env or dict(os.environ)))

    def test_healthy_gateway_orders_cron_then_memory_then_checks(self) -> None:
        def dirty_status(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:4] == ["openclaw", "memory", "status", "--agent"]:
                return subprocess.CompletedProcess(
                    cmd, 0, stdout='[{"status": {"files": 3, "dirty": true}}]', stderr=""
                )
            return self._ok(cmd)

        self.handler = dirty_status
        out, err = self.run_post_startup()
        self.assertEqual("", err)
        self.assertIn("Gateway healthy — running post-startup tasks", out)
        health_idx = self.index_of("openclaw", "health")
        status_idx = self.index_of("openclaw", "memory", "status")
        index_idx = next(
            i for i, c in enumerate(self.calls) if c[:2] == ["openclaw", "memory"] and "index" in c
        )
        validate_idx = self.index_of("openclaw", "config", "validate")
        # One shared doctor run feeds the skills reconcile and the lint
        # summary — it precedes the final config validate now.
        doctor_idx = max(i for i, c in enumerate(self.calls) if c[:2] == ["openclaw", "doctor"])
        self.assertLess(health_idx, status_idx)
        self.assertLess(status_idx, index_idx)
        self.assertLess(index_idx, doctor_idx)
        self.assertLess(doctor_idx, validate_idx)

    def test_seed_automations_invoked_in_process_with_spec_model(self) -> None:
        self.run_post_startup()
        self.assertEqual([["--model", "zai/glm-4.7"]], self.automation_argv)

    def test_cron_failure_is_non_fatal_warning(self) -> None:
        with mock.patch.object(entrypoint.seed_automations, "main", side_effect=SystemExit(1)):
            _out, err = self.run_post_startup()
        self.assertIn("cron seeding failed (exit 1, non-fatal)", err)

    def test_unhealthy_gateway_skips_everything(self) -> None:
        def unhealthy(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "health"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="down")
            return self._ok(cmd)

        self.handler = unhealthy
        ticks = iter(range(0, 10**6, 5))

        with (
            mock.patch.object(time, "monotonic", side_effect=lambda: next(ticks)),
            mock.patch.object(time, "sleep"),
        ):
            _out, err = self.run_post_startup()
        self.assertIn("gateway did not become healthy within 180s", err)
        self.assertEqual([], self.automation_argv)
        self.assertFalse(self.has_call("openclaw", "memory", "status"))

    def test_memory_reindex_env_off_skips_ladder(self) -> None:
        out, _err = self.run_post_startup({"AGENT_MEMORY_REINDEX": "0"})
        self.assertIn("memory reindex: skipped (AGENT_MEMORY_REINDEX=0)", out)
        self.assertFalse(self.has_call("openclaw", "memory", "status"))

    def test_stale_reindex_locks_removed_before_status(self) -> None:
        lock = self.data / "agents" / "main" / "agent" / "stale.reindex-lock.sqlite"
        lock.parent.mkdir(parents=True)
        lock.write_text("", encoding="utf-8")
        keeper = self.data / "agents" / "main" / "agent" / "other.sqlite"
        keeper.write_text("", encoding="utf-8")
        out, _err = self.run_post_startup()
        self.assertIn("removing 1 stale reindex lock(s)", out)
        self.assertFalse(lock.exists())
        self.assertTrue(keeper.exists())

    def test_doctor_findings_disable_unavailable_skills(self) -> None:
        findings = {
            "findings": [
                {
                    "checkId": "core/doctor/skills-readiness",
                    "path": "skills.entries.propose-doc-edit.enabled",
                },
                {
                    "checkId": "core/doctor/something-else",
                    "path": "skills.entries.other.enabled",
                },
            ]
        }

        def doctor(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "doctor"]:
                return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(findings), stderr="")
            return self._ok(cmd)

        self.handler = doctor
        self.run_post_startup()
        self.assertEqual(
            [
                [
                    "openclaw",
                    "config",
                    "set",
                    "skills.entries.propose-doc-edit.enabled",
                    "false",
                ]
            ],
            self.calls_with("openclaw", "config", "set"),
        )

    def test_doctor_findings_with_nonzero_exit_still_disable(self) -> None:
        # doctor --lint exits 1 iff any finding exists (verified against the
        # pinned image) — the old returncode guard made this the dead-code
        # path: findings were present exactly when the parse was skipped.
        findings = {
            "findings": [
                {
                    "checkId": "core/doctor/skills-readiness",
                    "path": "skills.entries.propose-doc-edit.enabled",
                }
            ]
        }

        def doctor(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "doctor"]:
                return subprocess.CompletedProcess(cmd, 1, stdout=json.dumps(findings), stderr="")
            return self._ok(cmd)

        self.handler = doctor
        self.run_post_startup()
        self.assertEqual(
            [
                [
                    "openclaw",
                    "config",
                    "set",
                    "skills.entries.propose-doc-edit.enabled",
                    "false",
                ]
            ],
            self.calls_with("openclaw", "config", "set"),
        )

    def test_healed_skill_is_reenabled(self) -> None:
        # A skill the doctor disabled in an earlier boot (marker present)
        # whose finding has cleared goes back on — the old code was a
        # one-way ratchet.
        self.write_openclaw_config(
            json.dumps({"skills": {"entries": {"old-skill": {"enabled": False}}}})
        )
        self._write_doctor_marker("old-skill\n")

        self.run_post_startup()
        self.assertEqual(
            [
                [
                    "openclaw",
                    "config",
                    "set",
                    "skills.entries.old-skill.enabled",
                    "true",
                ]
            ],
            self.calls_with("openclaw", "config", "set"),
        )
        self.assertFalse((self.data / "doctor-disabled-skills").exists())

    def test_operator_disabled_skill_is_never_reenabled(self) -> None:
        # enabled=false WITHOUT the doctor marker is operator intent
        # (spec config entry or hand-managed openclaw.json) — the doctor
        # reconcile only undoes its own actions.
        self.write_openclaw_config(
            json.dumps({"skills": {"entries": {"operator-skill": {"enabled": False}}}})
        )

        self.run_post_startup()
        self.assertEqual([], self.calls_with("openclaw", "config", "set"))
        self.assertFalse((self.data / "doctor-disabled-skills").exists())

    def test_stale_marker_entry_without_config_is_dropped(self) -> None:
        self._write_doctor_marker("ghost-skill\n")

        self.run_post_startup()
        self.assertEqual([], self.calls_with("openclaw", "config", "set"))
        self.assertFalse((self.data / "doctor-disabled-skills").exists())

    def test_unparseable_doctor_output_is_inert_and_keeps_the_marker(self) -> None:
        self._write_doctor_marker("old-skill\n")

        def doctor(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "doctor"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="not json", stderr="")
            return self._ok(cmd)

        self.handler = doctor
        self.run_post_startup()
        self.assertEqual([], self.calls_with("openclaw", "config", "set"))
        self.assertTrue((self.data / "doctor-disabled-skills").exists())

    def test_config_validation_and_lint_are_warn_only(self) -> None:
        def failing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "config", "validate"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="bad")
            if cmd[:2] == ["openclaw", "doctor"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="lint")
            return self._ok(cmd)

        self.handler = failing
        _out, err = self.run_post_startup()
        self.assertIn("config validation found issues", err)
        self.assertIn("doctor lint found issues", err)


class BackupBeforeUpgrade(EntrypointTestCase):
    """X1a: verified backup before an image-version delta touches a warm
    volume (`openclaw backup create --verify --output <dir>`, verified at
    the pinned tag). Fail-closed: a failed backup aborts the boot — data
    safety outranks gateway availability for a migration event."""

    def _marker(self) -> Path:
        return self.data / "last-image-version"

    def _delta_boot(self, version: str) -> None:
        self.data.mkdir(parents=True, exist_ok=True)
        entrypoint.backup_before_upgrade(
            {
                "AGENT_BASE_VERSION": version,
                "AGENT_BACKUP_DIR": str(self.data / "backups"),
            }
        )

    def test_dev_boot_without_version_skips_entirely(self) -> None:
        self.write_openclaw_config("{}")
        entrypoint.backup_before_upgrade({})
        self.assertEqual([], self.calls_with("openclaw", "backup"))
        self.assertFalse(self._marker().exists())

    def test_fresh_volume_writes_marker_without_backup(self) -> None:
        self._delta_boot("1.0")
        self.assertEqual([], self.calls_with("openclaw", "backup"))
        self.assertEqual("1.0", self._marker().read_text(encoding="utf-8").strip())

    def test_same_version_warm_volume_skips(self) -> None:
        self.write_openclaw_config("{}")
        self._marker().write_text("1.0", encoding="utf-8")
        self._delta_boot("1.0")
        self.assertEqual([], self.calls_with("openclaw", "backup"))
        self.assertEqual("1.0", self._marker().read_text(encoding="utf-8").strip())

    def test_version_delta_warm_volume_backs_up_and_updates_marker(self) -> None:
        self.write_openclaw_config("{}")
        self._marker().write_text("1.0", encoding="utf-8")
        self._delta_boot("2.0")
        call = self.calls_with("openclaw", "backup", "create")[0]
        self.assertIn("--verify", call)
        self.assertTrue(str(call[call.index("--output") + 1]).endswith("/backups"))
        self.assertEqual("2.0", self._marker().read_text(encoding="utf-8").strip())

    def test_missing_marker_on_warm_volume_counts_as_delta(self) -> None:
        # Volumes migrating from older images have no marker — they get the
        # backup on their first boot on a versioned image.
        self.write_openclaw_config("{}")
        self._delta_boot("2.0")
        self.assertEqual(1, len(self.calls_with("openclaw", "backup", "create")))

    def test_backup_failure_aborts_and_keeps_marker(self) -> None:
        self.write_openclaw_config("{}")
        self._marker().write_text("1.0", encoding="utf-8")

        def failing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "backup", "create"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="disk full")
            return self._ok(cmd)

        self.handler = failing
        err = io.StringIO()
        with self.assertRaises(SystemExit) as ctx, redirect_stderr(err):
            self._delta_boot("2.0")
        self.assertEqual(1, ctx.exception.code)
        self.assertIn("upgrade backup failed", err.getvalue())
        self.assertEqual("1.0", self._marker().read_text(encoding="utf-8").strip())

    def test_backup_timeout_aborts(self) -> None:
        self.write_openclaw_config("{}")
        self._marker().write_text("1.0", encoding="utf-8")

        def hanging(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "backup", "create"]:
                raise subprocess.TimeoutExpired(cmd, 600)
            return self._ok(cmd)

        self.handler = hanging
        with self.assertRaises(SystemExit):
            self._delta_boot("2.0")

    def test_backup_dir_override_respected(self) -> None:
        self.write_openclaw_config("{}")
        self._marker().write_text("1.0", encoding="utf-8")
        custom = self.data / "custom-backups"
        entrypoint.backup_before_upgrade(
            {
                "AGENT_BASE_VERSION": "2.0",
                "AGENT_BACKUP_DIR": str(custom),
            }
        )
        call = self.calls_with("openclaw", "backup", "create")[0]
        self.assertEqual(str(custom), call[call.index("--output") + 1])
        self.assertEqual("2.0", self._marker().read_text(encoding="utf-8").strip())


class ManagedMcpRemoval(EntrypointTestCase):
    """X1c: de-specified MCP servers are removed — but only ones the base
    itself registered (marker `{data}/agent-managed-mcp`), never operator-
    additions, and never spec servers merely skipped by if_env."""

    SPEC: dict[str, object] = {
        **copy.deepcopy(MINIMAL_SPEC),
        "mcp_servers": [
            {"name": "kept", "command": "tool"},
            {"name": "guarded", "command": "tool", "if_env": ["NEEDED_KEY"]},
        ],
    }

    def _marker(self) -> Path:
        return self.data / "agent-managed-mcp"

    def _write_marker(self, names: list[str]) -> None:
        self.data.mkdir(parents=True, exist_ok=True)
        self._marker().write_text(json.dumps(names), encoding="utf-8")

    def _registered(self, cmd: list[str]) -> subprocess.CompletedProcess[str]:
        if cmd[:3] == ["openclaw", "mcp", "list"]:
            return subprocess.CompletedProcess(
                cmd,
                0,
                stdout=json.dumps({"servers": ["kept", "guarded", "stale", "manual"]}),
                stderr="",
            )
        return self._ok(cmd)

    def test_despecified_managed_server_is_unset(self) -> None:
        self._write_marker(["kept", "guarded", "stale"])
        self.handler = self._registered
        spec = self.load_spec_with(self.SPEC)
        out, _err = self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertEqual(
            [["openclaw", "mcp", "unset", "stale"]],
            self.calls_with("openclaw", "mcp", "unset"),
        )
        self.assertIn("Removed MCP server 'stale'", out)
        self.assertEqual(
            ["kept", "guarded"], json.loads(self._marker().read_text(encoding="utf-8"))
        )

    def test_if_env_guarded_server_is_never_unset(self) -> None:
        # 'guarded' is in the spec (env merely unsatisfied this boot) — it
        # must not be treated as de-specified.
        self._write_marker(["kept", "guarded"])
        self.handler = self._registered
        spec = self.load_spec_with(self.SPEC)
        self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertEqual([], self.calls_with("openclaw", "mcp", "unset"))

    def test_manual_server_never_touched(self) -> None:
        # 'manual' is registered but was never base-managed (absent from
        # the marker) — removal must not consider it.
        self._write_marker(["kept"])
        self.handler = self._registered
        spec = self.load_spec_with(self.SPEC)
        self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertEqual([], self.calls_with("openclaw", "mcp", "unset"))

    def test_unset_failure_warns_and_keeps_marker_entry(self) -> None:
        self._write_marker(["kept", "stale"])

        def failing_unset(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "mcp", "unset"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="locked")
            return self._registered(cmd)

        self.handler = failing_unset
        spec = self.load_spec_with(self.SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertIn("mcp unset failed: stale", err)
        self.assertEqual(
            ["kept", "guarded", "stale"],
            json.loads(self._marker().read_text(encoding="utf-8")),
        )

    def test_already_gone_server_dropped_from_marker_silently(self) -> None:
        self._write_marker(["kept", "ghost"])

        def listing(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "mcp", "list"]:
                return subprocess.CompletedProcess(
                    cmd, 0, stdout=json.dumps({"servers": ["kept"]}), stderr=""
                )
            return self._ok(cmd)

        self.handler = listing
        spec = self.load_spec_with(self.SPEC)
        self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertEqual([], self.calls_with("openclaw", "mcp", "unset"))
        self.assertEqual(
            ["kept", "guarded"], json.loads(self._marker().read_text(encoding="utf-8"))
        )

    def test_first_boot_writes_marker_with_spec_names(self) -> None:
        self.handler = self._registered
        spec = self.load_spec_with(self.SPEC)
        self.capture(lambda: entrypoint.reconcile_mcp(spec, os.environ))
        self.assertEqual([], self.calls_with("openclaw", "mcp", "unset"))
        self.assertEqual(
            ["kept", "guarded"], json.loads(self._marker().read_text(encoding="utf-8"))
        )


class PluginOrphanReport(EntrypointTestCase):
    """X1d: registry plugins installed but absent from the spec surface as
    a warn-only report. The base's own installs are recorded at first boot
    ({data}/agent-managed-plugins) — bundled and marker plugins are never
    orphans, and a missing marker (older-image volume) disables the report."""

    SPEC: dict[str, object] = {
        **copy.deepcopy(MINIMAL_SPEC),
        "plugins": [{"name": "approvals"}],
    }

    def _marker(self) -> Path:
        return self.data / "agent-managed-plugins"

    def _write_base_marker(self, ids: list[str]) -> None:
        self.data.mkdir(parents=True, exist_ok=True)
        self._marker().write_text(json.dumps(ids), encoding="utf-8")

    def _listing(
        self, installed: list[dict[str, object]]
    ) -> Callable[[list[str]], subprocess.CompletedProcess[str]]:
        def handler(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "plugins", "list"]:
                return subprocess.CompletedProcess(
                    cmd, 0, stdout=json.dumps({"plugins": installed}), stderr=""
                )
            return self._ok(cmd)

        return handler

    def test_extra_registry_plugin_warns(self) -> None:
        self._write_base_marker(["zai", "llama-cpp"])
        self.handler = self._listing(
            [
                {"id": "approvals", "origin": "registry"},
                {"id": "old-plugin", "origin": "registry"},
            ]
        )
        spec = self.load_spec_with(self.SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertIn("plugin 'old-plugin' installed but not in spec", err)
        self.assertNotIn("approvals", err)

    def test_bundled_and_base_plugins_are_not_orphans(self) -> None:
        self._write_base_marker(["zai", "llama-cpp"])
        self.handler = self._listing(
            [
                {"id": "approvals", "origin": "registry"},
                {"id": "active-memory", "origin": "bundled"},
                {"id": "llama-cpp", "origin": "npm"},
                {"id": "zai", "origin": "registry"},
            ]
        )
        spec = self.load_spec_with(self.SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertNotIn("not in spec", err)

    def test_missing_marker_disables_report(self) -> None:
        # Volumes from older images have no marker — never mis-attribute.
        self.handler = self._listing([{"id": "old-plugin", "origin": "registry"}])
        spec = self.load_spec_with(self.SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertNotIn("not in spec", err)

    def test_unparseable_listing_stays_silent(self) -> None:
        self._write_base_marker([])

        def bad(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "plugins", "list"]:
                return subprocess.CompletedProcess(cmd, 0, stdout="not json", stderr="")
            return self._ok(cmd)

        self.handler = bad
        spec = self.load_spec_with(self.SPEC)
        _out, err = self.capture(lambda: entrypoint.reconcile_plugins(spec))
        self.assertNotIn("not in spec", err)

    def test_first_boot_snapshots_base_plugins(self) -> None:
        self.handler = self._listing(
            [
                {"id": "active-memory", "origin": "bundled"},
                {"id": "zai", "origin": "registry"},
                {"id": "llama-cpp", "origin": "npm"},
            ]
        )
        spec = self.load_spec_with(self.SPEC)
        self.capture(lambda: entrypoint.first_boot_setup(spec))
        self.assertEqual(
            ["zai", "llama-cpp"],
            json.loads(self._marker().read_text(encoding="utf-8")),
        )


class ToolsDenyDefault(EntrypointTestCase):
    """X2: the base applies a default tools.deny (recursion/spawn surfaces)
    unless the spec configures tools itself — the agent's own chat turns
    must not reach the cron tool (self-replication) or spawn chains.
    heartbeat_respond is deliberately NOT denied (heartbeat delivery)."""

    def test_default_applied_when_spec_silent(self) -> None:
        spec = self.load_default_spec()
        self.capture(lambda: entrypoint.reconcile_config(spec, os.environ))
        calls = self.calls_with("openclaw", "config", "set", "tools.deny")
        self.assertEqual(1, len(calls))
        self.assertEqual(
            ["cron", "subagents", "sessions_spawn", "nodes"],
            json.loads(calls[0][calls[0].index("tools.deny") + 1]),
        )

    def test_spec_tools_deny_wins_over_base_default(self) -> None:
        spec_dict = copy.deepcopy(MINIMAL_SPEC)
        spec_dict["config"] = [{"path": "tools.deny", "value": "custom"}]
        spec = self.load_spec_with(spec_dict)
        self.capture(lambda: entrypoint.reconcile_config(spec, os.environ))
        calls = self.calls_with("openclaw", "config", "set", "tools.deny")
        self.assertEqual(1, len(calls))
        self.assertEqual("custom", calls[0][calls[0].index("tools.deny") + 1])

    def test_spec_tools_profile_also_disables_base_default(self) -> None:
        # Any explicit tools configuration in the spec signals operator
        # ownership of tool policy; the base default stands down entirely.
        spec_dict = copy.deepcopy(MINIMAL_SPEC)
        spec_dict["config"] = [{"path": "tools.profile", "value": "coding"}]
        spec = self.load_spec_with(spec_dict)
        self.capture(lambda: entrypoint.reconcile_config(spec, os.environ))
        self.assertEqual([], self.calls_with("openclaw", "config", "set", "tools.deny"))

    def test_post_startup_passes_default_tools_flag(self) -> None:
        spec_dict = copy.deepcopy(MINIMAL_SPEC)
        spec_dict["automations"] = {
            "model": "zai/glm-4.7",
            "default_tools": ["read"],
        }
        spec = self.load_spec_with(spec_dict)
        self.capture(lambda: entrypoint.post_startup(spec, os.environ))
        self.assertEqual(1, len(self.automation_argv))
        self.assertIn("--default-tools", self.automation_argv[0])
        self.assertEqual(
            "read", self.automation_argv[0][self.automation_argv[0].index("--default-tools") + 1]
        )

    def test_post_startup_omits_flag_without_spec_default(self) -> None:
        spec = self.load_default_spec()
        self.capture(lambda: entrypoint.post_startup(spec, os.environ))
        self.assertEqual(1, len(self.automation_argv))
        self.assertNotIn("--default-tools", self.automation_argv[0])


class PostStartupDiagnostics(PostStartupFlow):
    """X7: post_startup surfaces doctor counts, persists reports when
    findings exist, runs a warn-only security audit, and writes a boot
    status file ({data}/status.json) with the warning count. Doctor runs
    ONCE per boot — the skills reconcile and the lint warn share it."""

    def setUp(self) -> None:
        super().setUp()
        entrypoint._boot_warnings.clear()

    def _doctor(
        self, findings: list[dict[str, str]]
    ) -> Callable[[list[str]], subprocess.CompletedProcess[str]]:
        def handler(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "doctor"]:
                payload = {
                    "ok": not findings,
                    "checksRun": 24,
                    "checksSkipped": 27,
                    "findings": findings,
                }
                code = 1 if findings else 0
                return subprocess.CompletedProcess(cmd, code, stdout=json.dumps(payload), stderr="")
            if cmd[:2] == ["openclaw", "security"]:
                return subprocess.CompletedProcess(cmd, 0, stdout="[]", stderr="")
            if cmd[:3] == ["openclaw", "memory", "status"]:
                clean_memory = (
                    '[{"status": {"files": 0, "dirty": false, '
                    '"custom": {"indexIdentity": {"status": "valid"}}}}]'
                )
                return subprocess.CompletedProcess(cmd, 0, stdout=clean_memory, stderr="")
            return self._ok(cmd)

        return handler

    def test_doctor_findings_surfaced_and_report_persisted(self) -> None:
        findings = [
            {"checkId": "core/doctor/gateway-auth", "severity": "warning", "path": "gateway.auth"}
        ]
        self.handler = self._doctor(findings)
        self.run_post_startup()
        report = self.data / "logs" / "doctor-report.json"
        self.assertTrue(report.exists())
        persisted = json.loads(report.read_text(encoding="utf-8"))
        self.assertEqual("core/doctor/gateway-auth", persisted["findings"][0]["checkId"])

    def test_clean_doctor_leaves_no_report(self) -> None:
        self.handler = self._doctor([])
        self.run_post_startup()
        self.assertFalse((self.data / "logs" / "doctor-report.json").exists())

    def test_doctor_runs_once_per_boot(self) -> None:
        self.handler = self._doctor([])
        self.run_post_startup()
        self.assertEqual(1, len(self.calls_with("openclaw", "doctor")))

    def test_security_findings_persisted(self) -> None:
        def handler(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "security"]:
                payload = [{"checkId": "gateway.trusted_proxies_missing", "severity": "warn"}]
                return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(payload), stderr="")
            return self._doctor([])(cmd)

        self.handler = handler
        self.run_post_startup()
        report = self.data / "logs" / "security-report.json"
        self.assertTrue(report.exists())
        self.assertEqual(
            "gateway.trusted_proxies_missing",
            json.loads(report.read_text(encoding="utf-8"))["findings"][0]["checkId"],
        )

    def test_status_file_counts_warnings(self) -> None:
        def noisy(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "config", "validate"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="")
            return self._doctor([])(cmd)

        self.handler = noisy
        self.run_post_startup()
        status = json.loads((self.data / "status.json").read_text(encoding="utf-8"))
        self.assertGreaterEqual(status["warnings"], 1)
        self.assertIn("imageVersion", status)

    def test_clean_boot_status_has_zero_warnings(self) -> None:
        self.handler = self._doctor([])
        self.run_post_startup()
        status = json.loads((self.data / "status.json").read_text(encoding="utf-8"))
        self.assertEqual(0, status["warnings"])

    def test_unparseable_security_output_is_inert(self) -> None:
        def handler(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:2] == ["openclaw", "security"]:
                return subprocess.CompletedProcess(cmd, 0, stdout="not json", stderr="")
            return self._doctor([])(cmd)

        self.handler = handler
        self.run_post_startup()
        self.assertFalse((self.data / "logs" / "security-report.json").exists())


class MainFlow(EntrypointTestCase):
    def test_full_boot_orders_phases_and_hands_off_via_execvp(self) -> None:
        result = self.boot()
        self.assertEqual(0, result.code)
        result.fork.assert_called_once_with()
        result.execvp.assert_called_once_with("openclaw", ["openclaw", "gateway"])
        setup_idx = self.index_of("openclaw", "setup")
        config_idx = self.index_of("openclaw", "config", "set")
        self.assertLess(setup_idx, config_idx)
        self.assertTrue((self.data / "workspace" / "journal").is_dir())

    def test_warm_boot_reconciles_but_never_sets_up(self) -> None:
        self.write_openclaw_config("{}")
        self.boot()
        self.assertFalse(self.has_call("openclaw", "setup"))
        self.assertTrue(self.has_call("openclaw", "config", "set"))

    def test_manage_config_zero_skips_reconcile_but_not_first_boot_or_seeding(self) -> None:
        with mock.patch.dict(os.environ, {"AGENT_MANAGE_CONFIG": "0"}):
            result = self.boot()
        self.assertEqual(0, result.code)
        self.assertFalse(self.has_call("openclaw", "config", "set"))
        self.assertFalse(self.has_call("openclaw", "mcp"))
        # The only plugins list is first boot's ownership snapshot — the
        # reconcile-side orphan report never runs under AGENT_MANAGE_CONFIG=0.
        self.assertEqual(1, len(self.calls_with("openclaw", "plugins", "list", "--json")))
        self.assertTrue(self.has_call("openclaw", "setup"))
        # First boot installs the llama-cpp provider (the only plugins
        # install in this boot: the default spec declares no plugins).
        self.assertTrue(self.has_call("openclaw", "plugins", "install"))
        self.assertTrue((self.data / "workspace" / "journal").is_dir())

    def test_skip_seed_still_reconciles_config(self) -> None:
        with mock.patch.dict(os.environ, {"AGENT_SKIP_SEED": "1"}):
            result = self.boot()
        self.assertEqual(0, result.code)
        self.assertTrue(self.has_call("openclaw", "config", "set", "channels.telegram.dmPolicy"))
        self.assertFalse((self.data / "workspace").exists())

    def test_child_branch_runs_post_startup_and_exits_without_exec(self) -> None:
        result = self.boot_child()
        self.assertEqual([0], result.exit_codes)
        result.execvp.assert_not_called()
        self.assertTrue(self.has_call("openclaw", "health"))
        self.assertEqual([["--model", "zai/glm-4.7"]], self.automation_argv)

    def test_missing_command_exits_two(self) -> None:
        self.assertEqual(2, entrypoint.main([]))

    def test_spec_error_aborts_boot_loudly(self) -> None:
        bad = copy.deepcopy(MINIMAL_SPEC)
        bad["specVersion"] = 99
        self._write_spec(bad)
        with self.assertRaises(entrypoint.SpecError):
            entrypoint.main(["openclaw", "gateway"])
        self.assertEqual([], self.calls)


class ValidateSpecMode(EntrypointTestCase):
    def validate(self) -> tuple[int, str, str]:
        code_holder: list[int] = []

        def run_validate() -> None:
            code_holder.append(entrypoint.main(["--validate-spec"]))

        out, err = self.capture(run_validate)
        return code_holder[0], out, err

    def test_freya_fixture_validates_without_mutation(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "AGENT_SPEC_PATH": str(FIXTURES / "freya-like" / "spec.json"),
                "AGENT_AUTOMATIONS_DIR": str(FIXTURES / "freya-like" / "automations"),
                **FREYA_ENV,
            },
        ):
            code, out, err = self.validate()
        self.assertEqual(0, code)
        self.assertEqual("", err)
        self.assertIn("spec validation passed", out)
        self.assertEqual([], self.calls)

    def test_mimir_fixture_validates_without_mutation(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "AGENT_SPEC_PATH": str(FIXTURES / "mimir-like" / "spec.json"),
                "AGENT_AUTOMATIONS_DIR": str(FIXTURES / "mimir-like" / "automations"),
                **MIMIR_ENV,
            },
        ):
            code, out, err = self.validate()
        self.assertEqual(0, code)
        self.assertEqual("", err)
        self.assertIn("spec validation passed", out)
        self.assertEqual([], self.calls)

    def test_unsupported_specversion_is_rejected(self) -> None:
        bad = copy.deepcopy(MINIMAL_SPEC)
        bad["specVersion"] = SPEC_VERSION_SUPPORTED + 1
        self._write_spec(bad)
        with mock.patch.dict(os.environ, {"AGENT_SPEC_PATH": str(self.home / "spec.json")}):
            code, _out, err = self.validate()
        self.assertEqual(1, code)
        self.assertIn("specVersion", err)
        self.assertEqual([], self.calls)

    def test_missing_required_env_is_rejected(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "AGENT_SPEC_PATH": str(FIXTURES / "freya-like" / "spec.json"),
                "AGENT_AUTOMATIONS_DIR": str(FIXTURES / "freya-like" / "automations"),
                **{k: v for k, v in FREYA_ENV.items() if k != "TELEGRAM_ALLOWED_USERS"},
            },
        ):
            code, _out, err = self.validate()
        self.assertEqual(1, code)
        self.assertIn("TELEGRAM_ALLOWED_USERS", err)

    def test_empty_automations_dir_is_rejected(self) -> None:
        empty = self.home / "empty-automations"
        empty.mkdir()
        with mock.patch.dict(os.environ, {"AGENT_AUTOMATIONS_DIR": str(empty)}):
            code, _out, err = self.validate()
        self.assertEqual(1, code)
        self.assertIn("no automation specs found", err)

    def test_missing_spec_file_is_rejected(self) -> None:
        with mock.patch.dict(os.environ, {"AGENT_SPEC_PATH": str(self.home / "nope.json")}):
            code, _out, err = self.validate()
        self.assertEqual(1, code)
        self.assertIn("spec validation failed", err)


class FixtureBoots(EntrypointTestCase):
    def boot_fixture(self, name: str, env: dict[str, str]) -> SimpleNamespace:
        fixture = FIXTURES / name
        with (
            mock.patch.dict(
                os.environ,
                {
                    "AGENT_SPEC_PATH": str(fixture / "spec.json"),
                    "AGENT_AUTOMATIONS_DIR": str(fixture / "automations"),
                    **env,
                },
            ),
            mock.patch.object(entrypoint, "SEED_BASE", fixture),
        ):
            return self.boot()

    def test_freya_like_full_boot(self) -> None:
        with mock.patch.object(entrypoint.shutil, "which", return_value="/usr/bin/gh"):
            result = self.boot_fixture("freya-like", FREYA_ENV)
        self.assertEqual(0, result.code)
        result.execvp.assert_called_once_with("openclaw", ["openclaw", "gateway"])

        self.assertTrue(self.has_call("openclaw", "setup"))
        self.assertEqual(
            [
                "openclaw",
                "config",
                "set",
                "channels.telegram.allowFrom",
                '["111", "222"]',
                "--strict-json",
            ],
            self.calls_with("openclaw", "config", "set", "channels.telegram.allowFrom")[0],
        )
        self.assertTrue(
            self.has_call("openclaw", "config", "set", "agents.defaults.heartbeat.to", '"-100123"')
        )
        self.assertTrue(
            self.has_call(
                "openclaw",
                "config",
                "set",
                "plugins.entries.grow-approval-gate.config.gatedTools",
                '["set_port_mode", "set_stage_thresholds", "calibrate_sensor"]',
            )
        )
        self.assertEqual(
            [
                "openclaw",
                "mcp",
                "add",
                "ac-infinity",
                "--command",
                "ac-infinity-mcp",
                "--env",
                "AC_INFINITY_EMAIL=grower@example.com",
                "--env",
                "AC_INFINITY_PASSWORD=ac-secret",
                "--no-probe",
                "--timeout",
                "60",
            ],
            self.calls_with("openclaw", "mcp", "add", "ac-infinity")[0],
        )
        self.assertTrue(
            self.has_call("openclaw", "plugins", "install", "/opt/seed/plugins/grow-approval-gate")
        )
        self.assertEqual([["gh", "auth", "login", "--with-token"]], self.calls_with("gh"))
        self.assertEqual(["ghp-freya-token"], self.stdin_inputs)
        self.assertEqual(
            "# freya-like\n\nGrow-tent agent fixture for the standard-agent base image.\n",
            (self.data / "workspace" / "AGENTS.md").read_text("utf-8"),
        )
        self.assertTrue((self.data / "skills" / ".keep").is_file())

    def test_freya_like_fails_closed_without_declared_env(self) -> None:
        # heartbeat.to templates {env:TELEGRAM_CHAT_ID}; templates resolve
        # at load time even for if_env-guarded entries, so a boot without
        # the declared env aborts loudly instead of running half-configured.
        env = {k: v for k, v in FREYA_ENV.items() if k != "TELEGRAM_CHAT_ID"}
        fixture = FIXTURES / "freya-like"
        with (
            mock.patch.dict(
                os.environ,
                {
                    "AGENT_SPEC_PATH": str(fixture / "spec.json"),
                    "AGENT_AUTOMATIONS_DIR": str(fixture / "automations"),
                    **env,
                },
            ),
            mock.patch.object(entrypoint, "SEED_BASE", fixture),
            self.assertRaises(entrypoint.SpecError) as ctx,
        ):
            self.boot()
        self.assertIn("TELEGRAM_CHAT_ID", str(ctx.exception))
        self.assertEqual([], self.calls)

    def test_mimir_like_full_boot_registers_all_six_servers(self) -> None:
        result = self.boot_fixture("mimir-like", MIMIR_ENV)
        self.assertEqual(0, result.code)
        added = [c[3] for c in self.calls_with("openclaw", "mcp", "add")]
        self.assertEqual(
            ["trade-agent", "defillama", "tradingview", "alpha-vantage", "lunarcrush", "postgres"],
            added,
        )
        self.assertEqual(
            [
                "openclaw",
                "mcp",
                "add",
                "alpha-vantage",
                "--url",
                "https://mcp.alphavantage.co/mcp?apikey=av-key",
                "--no-probe",
            ],
            self.calls_with("openclaw", "mcp", "add", "alpha-vantage")[0],
        )
        self.assertEqual(
            [
                "openclaw",
                "mcp",
                "add",
                "lunarcrush",
                "--url",
                "https://lunarcrush.ai/mcp",
                "--header",
                "Authorization=Bearer lc-key",
                "--no-probe",
            ],
            self.calls_with("openclaw", "mcp", "add", "lunarcrush")[0],
        )
        # gh_auth false and no local plugins: neither path runs.
        self.assertEqual([], self.calls_with("gh"))
        self.assertFalse(self.has_call("openclaw", "plugins", "install", "/opt/seed"))
        self.assertTrue((self.data / "workspace" / "AGENTS.md").is_file())
        self.assertFalse((self.data / "skills").exists())

    def test_mimir_like_boot_phase_order(self) -> None:
        self.boot_fixture("mimir-like", MIMIR_ENV)
        config_idx = self.index_of("openclaw", "config", "set")
        llama_idx = self.index_of("openclaw", "plugins", "install")
        mcp_idx = self.index_of("openclaw", "mcp", "add", "trade-agent")
        self.assertLess(config_idx, mcp_idx)
        self.assertLess(llama_idx, mcp_idx)


class SecretsCanary(EntrypointTestCase):
    SECRET = "canary-ZZZ-never-for-logs"

    def canary_spec(self) -> dict[str, object]:
        spec = copy.deepcopy(MINIMAL_SPEC)
        spec["config"] = [{"path": "gateway.token", "value": "{env:SECRET_TOKEN}", "strict": True}]
        spec["mcp_servers"] = [
            {"name": "sentinel", "url": "https://mcp.example.com/mcp?key={env:SECRET_TOKEN}"}
        ]
        return spec

    def test_resolved_secret_reaches_cli_but_never_logs(self) -> None:
        self._write_spec(self.canary_spec())
        with mock.patch.dict(os.environ, {"SECRET_TOKEN": self.SECRET}):
            result = self.boot()
        self.assertEqual(0, result.code)
        combined = result.stdout + result.stderr
        self.assertNotIn(self.SECRET, combined)
        # ...while it did flow to the CLI (both the config set and the mcp add).
        secret_calls = [c for c in self.calls if self.SECRET in " ".join(c)]
        self.assertEqual(2, len(secret_calls))

    def test_failed_config_set_warns_without_leaking_secret(self) -> None:
        def failing_set(cmd: list[str]) -> subprocess.CompletedProcess[str]:
            if cmd[:3] == ["openclaw", "config", "set"]:
                return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="denied")
            return self._ok(cmd)

        self.handler = failing_set
        self._write_spec(self.canary_spec())
        with mock.patch.dict(os.environ, {"SECRET_TOKEN": self.SECRET}):
            result = self.boot()
        self.assertIn("config set failed: gateway.token", result.stderr)
        self.assertNotIn(self.SECRET, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
