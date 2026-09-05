#!/usr/bin/env python3
# allow: SIZE_OK — contractually a single test file (exactly two files land
# in this repo); one suite per behavior class for the reconciler module.
"""Merged unittest suite for the standard-agent seed_automations module.

Union of the freya and mimir suites, rebased onto the standard contract:

- LoaderSchema — fail-closed header parsing (unknown key/token, duplicate
  key/name, name/stem mismatch, every-xor-cron, deliver enum, bad
  topic-env, empty dir/body), token substitution, and single-line
  canonical rendering. All against throwaway temp directories.
- ModelResolution — the --model / AUTOMATION_MODEL ladder with no baked
  default (silent model drift forbidden); neither source aborts.
- JobModelPolicy — the per-job `model:` header: overrides the global
  model on `cron add`; empty declaration fails closed; stored-model
  drift (payload.model, source-verified shape at the pinned base tag)
  heals via `cron edit --model`.
- TriggerScriptPolicy — the `trigger-script:` header: fail-closed
  loading (relative-path confinement, missing/empty/oversized scripts),
  the AGENT_AUTOMATION_TRIGGERS opt-in gate + config arming, stored
  trigger.script currency (source-verified shape at the pinned base
  tag), and add/edit/clear argv shapes.
- EnvContract — the standard env names: AGENT_AUTOMATIONS_DIR override
  (default <script dir>/automations) and TELEGRAM_CHAT_ID.
- DurationParsing — unit/compound/bare-ms forms and rejections.
- ScheduleCurrent — precise kind-dict comparison (everyMs int with bool
  exclusion, cron expr whitespace-collapsed, server-added fields ignored).
- TolerantStoredShapes — the legacy fallback: plain-string schedules and
  dicts carrying expression/cron/every/value string keys compare
  whitespace-collapsed against the literal spec value; plus the
  anti-false-match lock (a stored cron-form for an every-spec is drift).
- JobCurrency / DeliveryFlags / ReconcileContract — message, threadId,
  chat, and schedule quadruple; delivery flag matrix; create/edit/skip/
  prune against a mocked CLI.
- MainFlow — one listing per run; spec error, model error, and unparseable
  listing all abort before any mutation; --model flows into cron add.

Project-specific payload-equivalence digest locks (per-project deployed
prompt bytes) were dropped — meaningless in the shared base.

Runs directly with no pytest dependency:

    python3 -m unittest discover -s container -p "test_seed_automations.py" -v
"""

from __future__ import annotations

import contextlib
import importlib
import io
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from subprocess import CompletedProcess
from unittest import mock

import seed_automations

# NOTE: always reference module members as seed_automations.X — never bind
# them with `from ... import` — because EnvContract reloads the module to
# pin import-time env reads, which rebinds every class/function to fresh
# objects. Stale from-imports would make assertRaises mismatch classes.

VALID_SPEC = """---
name: test-job
every: 15m
deliver: announce
topic-env: TELEGRAM_TOPIC_TEST
---
Check {{JOURNAL}}/tent-state.json and {{DOCS}}/automation/growth-stage-settings.md.
"""

DEFAULT_AUTOMATIONS_DIR = Path(seed_automations.__file__).resolve().parent / "automations"


def make_spec(
    flag: str = "--every",
    value: str = "15m",
    prompt: str = "do things",
    topic: str = "",
    deliver: str = "announce",
    model: str = "",
    trigger_path: Path | None = None,
    trigger_script: str = "",
) -> seed_automations.JobSpec:
    return seed_automations.JobSpec(
        name="test-job",
        schedule_flag=flag,
        schedule_value=value,
        prompt=prompt,
        topic=topic,
        deliver=deliver,
        model=model,
        trigger_path=trigger_path,
        trigger_script=trigger_script,
    )


def stored_job(
    schedule: object, message: str = "do things", model: str = "m", **overrides: object
) -> dict:
    job: dict = {
        "id": "job-1",
        "name": "test-job",
        "schedule": schedule,
        "payload": {"kind": "agentTurn", "message": message, "model": model},
        "delivery": {},
        # Currency now includes tool policy and model: the default-bounded
        # fixture carrying the default global model.
        "toolsAllow": list(seed_automations.DEFAULT_JOB_TOOLS),
    }
    job.update(overrides)
    return job


class ModuleImportSafety(unittest.TestCase):
    def test_main_guarded_by_name_main(self) -> None:
        source = Path(seed_automations.__file__).read_text(encoding="utf-8")
        self.assertIn('if __name__ == "__main__":', source)

    def test_import_exposes_main_without_loading_specs(self) -> None:
        # Reaching this assertion proves the module import above did not
        # parse spec files or shell out to `openclaw` (the loader runs
        # inside build_jobs()/main(), never at import time).
        self.assertTrue(callable(seed_automations.main))


class ModelResolution(unittest.TestCase):
    """--model wins over AUTOMATION_MODEL; no baked default exists — an
    empty resolution is a hard error naming both sources."""

    def test_cli_model_wins_over_env(self) -> None:
        with mock.patch.dict(os.environ, {"AUTOMATION_MODEL": "env/model"}):
            self.assertEqual(seed_automations.resolve_model("cli/model"), "cli/model")

    def test_env_model_used_when_cli_absent(self) -> None:
        with mock.patch.dict(os.environ, {"AUTOMATION_MODEL": "env/model"}):
            self.assertEqual(seed_automations.resolve_model(""), "env/model")
            self.assertEqual(seed_automations.resolve_model(None), "env/model")

    def test_neither_source_raises_naming_both(self) -> None:
        with mock.patch.dict(os.environ):
            os.environ.pop("AUTOMATION_MODEL", None)
            with self.assertRaises(seed_automations.ModelResolutionError) as ctx:
                seed_automations.resolve_model("")
        message = str(ctx.exception)
        self.assertIn("--model", message)
        self.assertIn("AUTOMATION_MODEL", message)


class TempSpecDir(unittest.TestCase):
    """Base: a temp automations directory cleaned up per test."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)

    def write(self, filename: str, content: str) -> Path:
        path = self.dir / filename
        path.write_text(content, encoding="utf-8")
        return path

    def load(self) -> list[seed_automations.JobSpec]:
        return seed_automations.build_jobs(self.dir)


class LoaderSchema(TempSpecDir):
    """Fail-closed parsing of markdown job specs, against temp dirs."""

    def test_valid_spec_parses_with_token_substitution(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        with mock.patch.dict(os.environ, {"TELEGRAM_TOPIC_TEST": "42"}):
            specs = self.load()
        self.assertEqual(len(specs), 1)
        spec = specs[0]
        self.assertEqual(spec.name, "test-job")
        self.assertEqual(spec.schedule_flag, "--every")
        self.assertEqual(spec.schedule_value, "15m")
        self.assertEqual(spec.deliver, "announce")
        self.assertEqual(spec.topic, "42")
        self.assertEqual(
            spec.prompt,
            f"Check {seed_automations.JOURNAL}/tent-state.json and "
            f"{seed_automations.DOCS}/automation/growth-stage-settings.md.",
        )

    def test_topic_env_unset_yields_empty_topic(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        env = dict(os.environ)
        env.pop("TELEGRAM_TOPIC_TEST", None)
        with mock.patch.dict(os.environ, env, clear=True):
            specs = self.load()
        self.assertEqual(specs[0].topic, "")

    def test_whitespace_collapses_to_single_line(self) -> None:
        body = "One\nTwo.   Three\t\tFour\n\nFive."
        old = "Check {{JOURNAL}}/tent-state.json and {{DOCS}}/automation/growth-stage-settings.md."
        self.write("test-job.md", VALID_SPEC.replace(old, body))
        self.assertEqual(self.load()[0].prompt, "One Two. Three Four Five.")

    def test_cron_header_selects_cron_flag(self) -> None:
        self.write("test-job.md", VALID_SPEC.replace("every: 15m", "cron: 2 9 * * *"))
        spec = self.load()[0]
        self.assertEqual(spec.schedule_flag, "--cron")
        self.assertEqual(spec.schedule_value, "2 9 * * *")

    def test_missing_directory_fails_closed(self) -> None:
        with self.assertRaises(seed_automations.AutomationSpecError):
            seed_automations.build_jobs(self.dir / "does-not-exist")

    def test_empty_directory_fails_closed(self) -> None:
        with self.assertRaises(seed_automations.AutomationSpecError):
            self.load()

    def test_invalid_specs_fail_closed(self) -> None:
        cases: list[tuple[str, str, str]] = [
            (
                "no opening fence",
                "test-job.md",
                VALID_SPEC.replace("---\nname", "~~~\nname", 1),
            ),
            (
                "unclosed fence",
                "test-job.md",
                VALID_SPEC.replace("---\nCheck", "Check"),
            ),
            (
                "missing colon separator",
                "test-job.md",
                VALID_SPEC.replace("every: 15m", "every 15m"),
            ),
            (
                "unknown key",
                "test-job.md",
                VALID_SPEC.replace("deliver: announce\n", "model: gpt\n"),
            ),
            (
                "duplicate key",
                "test-job.md",
                VALID_SPEC.replace("deliver: announce\n", "deliver: announce\ndeliver: announce\n"),
            ),
            (
                "missing deliver",
                "test-job.md",
                VALID_SPEC.replace("deliver: announce\n", ""),
            ),
            (
                "bad deliver enum",
                "test-job.md",
                VALID_SPEC.replace("deliver: announce", "deliver: no_deliver"),
            ),
            (
                "both schedules",
                "test-job.md",
                VALID_SPEC.replace("every: 15m", "every: 15m\ncron: 2 9 * * *"),
            ),
            (
                "no schedule",
                "test-job.md",
                VALID_SPEC.replace("every: 15m\n", ""),
            ),
            (
                "empty schedule value",
                "test-job.md",
                VALID_SPEC.replace("every: 15m", "every:"),
            ),
            (
                "name stem mismatch",
                "test-job.md",
                VALID_SPEC.replace("name: test-job", "name: other-job"),
            ),
            (
                "bad name charset",
                "Test-Job.md",
                VALID_SPEC.replace("name: test-job", "name: Test Job"),
            ),
            (
                "bad topic-env",
                "test-job.md",
                VALID_SPEC.replace("TELEGRAM_TOPIC_TEST", "telegram-topic"),
            ),
            (
                "unknown token",
                "test-job.md",
                VALID_SPEC.replace("{{JOURNAL}}", "{{JOUNRAL}}"),
            ),
            (
                "lowercase token",
                "test-job.md",
                VALID_SPEC.replace("{{JOURNAL}}", "{{journal}}"),
            ),
            (
                "unrecognized token syntax",
                "test-job.md",
                VALID_SPEC.replace(
                    "growth-stage-settings.md.",
                    "growth-stage-settings.md. Then {{handle unknown}}.",
                ),
            ),
            (
                "empty body",
                "test-job.md",
                VALID_SPEC[: VALID_SPEC.index("---", 4) + 3] + "\n\n",
            ),
        ]
        for label, filename, content in cases:
            with self.subTest(case=label):
                self.write(filename, content)
                with self.assertRaises(seed_automations.AutomationSpecError):
                    self.load()
                (self.dir / filename).unlink()

    def test_duplicate_names_across_files_fail_closed(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        self.write(
            "test-job-copy.md",
            VALID_SPEC.replace("every: 15m", "cron: 3 9 * * *"),
        )
        with self.assertRaises(seed_automations.AutomationSpecError):
            self.load()


class EnvContract(unittest.TestCase):
    """The standard env names: AGENT_AUTOMATIONS_DIR for the spec
    directory override, TELEGRAM_CHAT_ID for delivery."""

    def test_agent_automations_dir_override_honored(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        spec_dir = Path(tmp.name)
        (spec_dir / "test-job.md").write_text(VALID_SPEC, encoding="utf-8")
        with mock.patch.dict(os.environ, {"AGENT_AUTOMATIONS_DIR": str(spec_dir)}):
            [spec] = seed_automations.build_jobs()
        self.assertEqual(spec.name, "test-job")

    @unittest.skipIf(
        DEFAULT_AUTOMATIONS_DIR.is_dir(),
        "default automations dir exists in this checkout",
    )
    def test_default_dir_is_script_sibling(self) -> None:
        with mock.patch.dict(os.environ):
            os.environ.pop("AGENT_AUTOMATIONS_DIR", None)
            with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
                seed_automations.build_jobs()
        self.assertIn(str(DEFAULT_AUTOMATIONS_DIR), str(ctx.exception))

    def test_chat_reads_telegram_chat_id(self) -> None:
        original = seed_automations.CHAT
        try:
            with mock.patch.dict(os.environ, {"TELEGRAM_CHAT_ID": "-100"}):
                importlib.reload(seed_automations)
            self.assertEqual(seed_automations.CHAT, "-100")
        finally:
            importlib.reload(seed_automations)
        self.assertEqual(seed_automations.CHAT, original)


class JobToolsPolicy(TempSpecDir):
    """X2: seeded jobs run with a bounded tool allow-list by default
    (--tools on cron add); per-job `tools:` overrides. Today omitting
    --tools stores no policy — functionally unrestricted, i.e. the cron
    tool can self-replicate jobs (OWASP ASI06 class)."""

    def test_default_tools_applied_when_job_omits_tools(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        with mock.patch.object(seed_automations, "DEFAULT_JOB_TOOLS", ("read", "exec")):
            specs = self.load()
        self.assertEqual(("read", "exec"), specs[0].tools)

    def test_job_tools_csv_overrides_default(self) -> None:
        spec_text = VALID_SPEC.replace(
            "deliver: announce", "deliver: announce\ntools: read,write,bundle-mcp"
        )
        self.write("test-job.md", spec_text)
        specs = self.load()
        self.assertEqual(("read", "write", "bundle-mcp"), specs[0].tools)

    def test_job_tools_star_means_unrestricted(self) -> None:
        spec_text = VALID_SPEC.replace("deliver: announce", "deliver: announce\ntools: *")
        self.write("test-job.md", spec_text)
        specs = self.load()
        self.assertEqual(("*",), specs[0].tools)

    def test_invalid_tools_values_fail_closed(self) -> None:
        for bad in ("", "read,,write", "read write", ","):
            with self.subTest(tools=bad):
                spec_text = VALID_SPEC.replace(
                    "deliver: announce", f"deliver: announce\ntools: {bad}"
                )
                self.write("test-job.md", spec_text)
                with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
                    self.load()
                self.assertIn("tools", str(ctx.exception))

    def test_cron_add_receives_tools_flag(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        with mock.patch.object(seed_automations, "DEFAULT_JOB_TOOLS", ("read", "exec")):
            specs = self.load()
        argv = seed_automations.cron_add_argv(specs[0], "model-x", [])
        self.assertIn("--tools", argv)
        self.assertEqual("read,exec", argv[argv.index("--tools") + 1])
        self.assertNotIn("*", argv)

    def test_unrestricted_job_omits_tools_flag_entirely(self) -> None:
        spec_text = VALID_SPEC.replace("deliver: announce", "deliver: announce\ntools: *")
        self.write("test-job.md", spec_text)
        specs = self.load()
        argv = seed_automations.cron_add_argv(specs[0], "model-x", [])
        self.assertNotIn("--tools", argv)

    def test_default_tools_flag_overrides_builtin(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        specs = self.load()
        argv = seed_automations.cron_add_argv(
            specs[0], "model-x", [], default_tools=("web_search",)
        )
        self.assertEqual("web_search", argv[argv.index("--tools") + 1])


class JobModelPolicy(TempSpecDir):
    """X4: per-job `model:` header overrides the global automation model
    (--model / AUTOMATION_MODEL) on `cron add`; empty declarations fail
    closed like every other header violation."""

    def test_job_model_header_parses(self) -> None:
        spec_text = VALID_SPEC.replace("deliver: announce", "deliver: announce\nmodel: zai/mini")
        self.write("test-job.md", spec_text)
        self.assertEqual("zai/mini", self.load()[0].model)

    def test_model_absent_means_undeclared(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        self.assertEqual("", self.load()[0].model)

    def test_empty_model_fails_closed(self) -> None:
        for bad in ("", " "):
            with self.subTest(model=bad):
                spec_text = VALID_SPEC.replace(
                    "deliver: announce", f"deliver: announce\nmodel: {bad}"
                )
                self.write("test-job.md", spec_text)
                with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
                    self.load()
                self.assertIn("model", str(ctx.exception))

    def test_add_uses_job_model_over_global(self) -> None:
        spec_text = VALID_SPEC.replace("deliver: announce", "deliver: announce\nmodel: zai/mini")
        self.write("test-job.md", spec_text)
        argv = seed_automations.cron_add_argv(self.load()[0], "global/model", [])
        self.assertEqual("zai/mini", argv[argv.index("--model") + 1])

    def test_add_falls_back_to_global_model(self) -> None:
        self.write("test-job.md", VALID_SPEC)
        argv = seed_automations.cron_add_argv(self.load()[0], "global/model", [])
        self.assertEqual("global/model", argv[argv.index("--model") + 1])


class TriggerScriptLoader(TempSpecDir):
    """`trigger-script:` header — fail-closed loading against the scripts
    dir sibling of the automations dir (AGENT_SCRIPTS_DIR overrides)."""

    SCRIPT = "json({ fire: true })"

    def write_trigger_spec(self, rel: str = "probe.js") -> None:
        spec_text = VALID_SPEC.replace(
            "deliver: announce", f"deliver: announce\ntrigger-script: {rel}"
        )
        self.write("test-job.md", spec_text)

    def write_script(self, name: str, content: str) -> Path:
        scripts = self.dir.parent / "scripts"
        scripts.mkdir(exist_ok=True)
        path = scripts / name
        path.write_text(content, encoding="utf-8")
        return path

    def scripts_root(self) -> Path:
        return self.dir.parent / "scripts"

    def test_trigger_header_parses_with_trimmed_content(self) -> None:
        self.write_trigger_spec()
        self.write_script("probe.js", f"\n\n{self.SCRIPT}\n\n")
        [spec] = self.load()
        self.assertEqual(self.SCRIPT, spec.trigger_script)
        self.assertEqual(self.scripts_root() / "probe.js", spec.trigger_path)

    def test_trigger_header_works_with_cron_schedule(self) -> None:
        spec_text = VALID_SPEC.replace("every: 15m", "cron: */20 * * * *").replace(
            "deliver: announce", "deliver: announce\ntrigger-script: probe.js"
        )
        self.write("test-job.md", spec_text)
        self.write_script("probe.js", self.SCRIPT)
        [spec] = self.load()
        self.assertEqual("--cron", spec.schedule_flag)
        self.assertEqual(self.SCRIPT, spec.trigger_script)

    def test_empty_trigger_script_value_fails_closed(self) -> None:
        for bad in ("", " "):
            with self.subTest(value=bad):
                self.write_trigger_spec(bad)
                with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
                    self.load()
                self.assertIn("trigger-script", str(ctx.exception))
                (self.dir / "test-job.md").unlink()

    def test_missing_script_file_fails_closed(self) -> None:
        self.write_trigger_spec("gone.js")
        with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
            self.load()
        message = str(ctx.exception)
        self.assertIn("gone.js", message)
        self.assertIn(str(self.scripts_root()), message)

    def test_blank_script_content_fails_closed(self) -> None:
        self.write_trigger_spec()
        self.write_script("probe.js", " \n\t ")
        with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
            self.load()
        self.assertIn("empty", str(ctx.exception))

    def test_oversized_script_fails_closed_at_cli_limit(self) -> None:
        self.write_trigger_spec()
        self.write_script("probe.js", "a" * (seed_automations.TRIGGER_SCRIPT_MAX_BYTES + 1))
        with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
            self.load()
        self.assertIn("exceeds", str(ctx.exception))

    def test_script_at_exact_byte_limit_parses(self) -> None:
        self.write_trigger_spec()
        self.write_script("probe.js", "a" * seed_automations.TRIGGER_SCRIPT_MAX_BYTES)
        [spec] = self.load()
        self.assertEqual("a" * seed_automations.TRIGGER_SCRIPT_MAX_BYTES, spec.trigger_script)

    def test_non_utf8_script_fails_closed(self) -> None:
        self.write_trigger_spec()
        scripts = self.scripts_root()
        scripts.mkdir(exist_ok=True)
        (scripts / "probe.js").write_bytes(b"\xff\xfe\x00")
        with self.assertRaises(seed_automations.AutomationSpecError):
            self.load()

    def test_escaping_paths_fail_closed(self) -> None:
        for bad in ("/abs/probe.js", "../probe.js", "sub/../../probe.js", "back\\slash.js"):
            with self.subTest(rel=bad):
                self.write_trigger_spec(bad)
                with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
                    self.load()
                self.assertIn("relative path", str(ctx.exception))
                (self.dir / "test-job.md").unlink()

    def test_subdirectory_script_resolves(self) -> None:
        self.write_trigger_spec("checks/probe.js")
        scripts = self.scripts_root() / "checks"
        scripts.mkdir(parents=True, exist_ok=True)
        (scripts / "probe.js").write_text(self.SCRIPT, encoding="utf-8")
        [spec] = self.load()
        self.assertEqual(self.scripts_root() / "checks" / "probe.js", spec.trigger_path)

    def test_agent_scripts_dir_override_honored(self) -> None:
        self.write_trigger_spec()
        other = self.dir / "elsewhere"
        other.mkdir()
        (other / "probe.js").write_text(self.SCRIPT, encoding="utf-8")
        with mock.patch.dict(os.environ, {"AGENT_SCRIPTS_DIR": str(other)}):
            [spec] = self.load()
        self.assertEqual(other / "probe.js", spec.trigger_path)

    def test_sibling_scripts_ignored_when_override_set(self) -> None:
        self.write_trigger_spec()
        self.write_script("probe.js", self.SCRIPT)
        other = self.dir / "elsewhere"
        other.mkdir()
        (other / "probe.js").write_text(self.SCRIPT, encoding="utf-8")
        with mock.patch.dict(os.environ, {"AGENT_SCRIPTS_DIR": str(other)}):
            [spec] = self.load()
        self.assertEqual(other / "probe.js", spec.trigger_path)


class TriggerScriptCurrency(unittest.TestCase):
    """Stored `trigger.script` participates in currency: the trimmed
    file bytes must match exactly, foreign `once` policy is drift, and a
    spec without the header owns jobs with no trigger at all."""

    def setUp(self) -> None:
        patcher = mock.patch.object(seed_automations, "CHAT", "")
        patcher.start()
        self.addCleanup(patcher.stop)

    def spec_with_trigger(self) -> seed_automations.JobSpec:
        return make_spec(
            trigger_path=Path("/opt/agent/scripts/probe.js"),
            trigger_script="json({ fire: true })",
        )

    def trigger(self, script: str = "json({ fire: true })", **extra: object) -> dict:
        return {"script": script, **extra}

    def test_no_trigger_spec_owns_untriggered_job(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000})
        self.assertTrue(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_no_trigger_spec_with_stored_trigger_is_drift(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, trigger=self.trigger())
        self.assertFalse(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_matching_script_is_current(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, trigger=self.trigger())
        self.assertTrue(seed_automations._job_is_current(job, self.spec_with_trigger(), "m"))

    def test_script_content_drift_is_drift(self) -> None:
        job = stored_job(
            {"kind": "every", "everyMs": 900000}, trigger=self.trigger("json({ fire: false })")
        )
        self.assertFalse(seed_automations._job_is_current(job, self.spec_with_trigger(), "m"))

    def test_missing_stored_trigger_is_drift(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000})
        self.assertFalse(seed_automations._job_is_current(job, self.spec_with_trigger(), "m"))

    def test_foreign_once_policy_is_drift(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, trigger=self.trigger(once=True))
        self.assertFalse(seed_automations._job_is_current(job, self.spec_with_trigger(), "m"))

    def test_opaque_stored_trigger_is_drift(self) -> None:
        for stored in ("json({ fire: true })", 42, [], {"script": 42}, None):
            with self.subTest(stored=stored):
                job = stored_job({"kind": "every", "everyMs": 900000}, trigger=stored)
                self.assertFalse(
                    seed_automations._job_is_current(job, self.spec_with_trigger(), "m")
                )


class TriggerScriptArgv(unittest.TestCase):
    """cron add carries --trigger-script <path>; cron edit re-asserts it
    or clears a stored trigger when the spec dropped the header."""

    def setUp(self) -> None:
        self.chat_patcher = mock.patch.object(seed_automations, "CHAT", "")
        self.chat_patcher.start()
        self.addCleanup(self.chat_patcher.stop)
        self.oc_patcher = mock.patch.object(seed_automations, "openclaw")
        self.oc = self.oc_patcher.start()
        self.addCleanup(self.oc_patcher.stop)
        self.oc.return_value = CompletedProcess(args=[], returncode=0, stdout="", stderr="")

    def spec_with_trigger(self) -> seed_automations.JobSpec:
        return make_spec(
            trigger_path=Path("/opt/agent/scripts/probe.js"),
            trigger_script="json({ fire: true })",
        )

    def test_add_carries_trigger_script_flag(self) -> None:
        argv = seed_automations.cron_add_argv(self.spec_with_trigger(), "m", [])
        self.assertIn("--trigger-script", argv)
        self.assertEqual("/opt/agent/scripts/probe.js", argv[argv.index("--trigger-script") + 1])

    def test_add_without_trigger_omits_flag(self) -> None:
        argv = seed_automations.cron_add_argv(make_spec(), "m", [])
        self.assertNotIn("--trigger-script", argv)

    def test_edit_reasserts_declared_trigger(self) -> None:
        argv = seed_automations.cron_edit_argv(
            self.spec_with_trigger(), "job-1", [], "m", stored_has_trigger=True
        )
        self.assertIn("--trigger-script", argv)
        self.assertNotIn("--clear-trigger", argv)

    def test_edit_clears_trigger_when_spec_dropped_header(self) -> None:
        argv = seed_automations.cron_edit_argv(
            make_spec(), "job-1", [], "m", stored_has_trigger=True
        )
        self.assertIn("--clear-trigger", argv)
        self.assertNotIn("--trigger-script", argv)

    def test_edit_without_trigger_surface_emits_neither_flag(self) -> None:
        argv = seed_automations.cron_edit_argv(
            make_spec(), "job-1", [], "m", stored_has_trigger=False
        )
        self.assertNotIn("--clear-trigger", argv)
        self.assertNotIn("--trigger-script", argv)

    def test_reconcile_heals_trigger_content_drift_via_edit(self) -> None:
        job = stored_job(
            {"kind": "every", "everyMs": 900000},
            trigger={"script": "json({ fire: false })"},
        )
        seed_automations.reconcile(self.spec_with_trigger(), [job], "m")
        self.oc.assert_called_once()
        edit_argv = self.oc.call_args[0][0]
        self.assertEqual(["cron", "edit", "job-1"], edit_argv[:3])
        self.assertIn("--trigger-script", edit_argv)

    def test_reconcile_clears_stored_trigger_when_header_dropped(self) -> None:
        job = stored_job(
            {"kind": "every", "everyMs": 900000}, trigger={"script": "json({ fire: true })"}
        )
        seed_automations.reconcile(make_spec(), [job], "m")
        self.oc.assert_called_once()
        edit_argv = self.oc.call_args[0][0]
        self.assertEqual(["cron", "edit", "job-1"], edit_argv[:3])
        self.assertIn("--clear-trigger", edit_argv)


class TriggerScriptMainFlow(unittest.TestCase):
    """The AGENT_AUTOMATION_TRIGGERS opt-in gate and config arming:
    un-gated trigger automations abort before any mutation; the armed
    path sets cron.triggers.enabled before touching any job."""

    def setUp(self) -> None:
        self.chat_patcher = mock.patch.object(seed_automations, "CHAT", "")
        self.chat_patcher.start()
        self.addCleanup(self.chat_patcher.stop)
        self.spec = make_spec(
            trigger_path=Path("/opt/agent/scripts/probe.js"),
            trigger_script="json({ fire: true })",
        )

    def test_unset_gate_aborts_before_any_mutation(self) -> None:
        stderr = io.StringIO()
        with (
            mock.patch.dict(os.environ),
            mock.patch.object(seed_automations, "build_jobs", return_value=[self.spec]),
            mock.patch.object(seed_automations, "list_cron_jobs") as lst,
            mock.patch.object(seed_automations, "openclaw") as oc,
            contextlib.redirect_stderr(stderr),
        ):
            os.environ.pop(seed_automations.TRIGGER_GATE_ENV, None)
            with self.assertRaises(SystemExit) as ctx:
                seed_automations.main(["--model", "zai/test-model"])
        self.assertEqual(1, ctx.exception.code)
        output = stderr.getvalue()
        self.assertIn(seed_automations.TRIGGER_GATE_ENV, output)
        lst.assert_not_called()
        oc.assert_not_called()

    def test_non_one_gate_value_aborts(self) -> None:
        with (
            mock.patch.dict(os.environ, {seed_automations.TRIGGER_GATE_ENV: "true"}),
            mock.patch.object(seed_automations, "build_jobs", return_value=[self.spec]),
            mock.patch.object(seed_automations, "list_cron_jobs") as lst,
            mock.patch.object(seed_automations, "openclaw") as oc,
            self.assertRaises(SystemExit) as ctx,
        ):
            seed_automations.main(["--model", "zai/test-model"])
        self.assertEqual(1, ctx.exception.code)
        lst.assert_not_called()
        oc.assert_not_called()

    def test_armed_gate_sets_config_then_adds_with_trigger(self) -> None:
        stdout = io.StringIO()
        with (
            mock.patch.dict(os.environ, {seed_automations.TRIGGER_GATE_ENV: "1"}),
            mock.patch.object(seed_automations, "build_jobs", return_value=[self.spec]),
            mock.patch.object(seed_automations, "list_cron_jobs", return_value=[]),
            mock.patch.object(
                seed_automations, "openclaw", return_value=CompletedProcess([], 0, "", "")
            ) as oc,
            contextlib.redirect_stdout(stdout),
        ):
            seed_automations.main(["--model", "zai/test-model"])
        arm_argv = oc.call_args_list[0][0][0]
        self.assertEqual(["config", "set", "cron.triggers.enabled", "true"], arm_argv)
        add_argv = oc.call_args_list[1][0][0]
        self.assertEqual(["cron", "add"], add_argv[:2])
        self.assertIn("--trigger-script", add_argv)
        self.assertIn("armed cron triggers", stdout.getvalue())

    def test_failed_arming_warns_but_proceeds_to_add(self) -> None:
        stdout = io.StringIO()
        with (
            mock.patch.dict(os.environ, {seed_automations.TRIGGER_GATE_ENV: "1"}),
            mock.patch.object(seed_automations, "build_jobs", return_value=[self.spec]),
            mock.patch.object(seed_automations, "list_cron_jobs", return_value=[]),
            mock.patch.object(seed_automations, "openclaw") as oc,
            contextlib.redirect_stdout(stdout),
        ):
            oc.side_effect = [
                CompletedProcess([], 1, "", "config write refused"),
                CompletedProcess([], 0, "", ""),
            ]
            seed_automations.main(["--model", "zai/test-model"])
        self.assertEqual(2, oc.call_count)
        self.assertEqual(["cron", "add"], oc.call_args_list[1][0][0][:2])
        self.assertIn("[warn]", stdout.getvalue())

    def test_gate_stays_inert_without_trigger_automations(self) -> None:
        with (
            mock.patch.dict(os.environ, {seed_automations.TRIGGER_GATE_ENV: "1"}),
            mock.patch.object(seed_automations, "build_jobs", return_value=[make_spec()]),
            mock.patch.object(seed_automations, "list_cron_jobs", return_value=[]),
            mock.patch.object(
                seed_automations, "openclaw", return_value=CompletedProcess([], 0, "", "")
            ) as oc,
        ):
            seed_automations.main(["--model", "zai/test-model"])
        oc.assert_called_once()
        self.assertEqual(["cron", "add"], oc.call_args[0][0][:2])


class FailureAlerts(unittest.TestCase):
    """X3: seeded jobs alert on failed/skipped runs when a chat target
    exists. `cron add` has no alert flags at the pinned tag — alerts attach
    via `cron edit` right after creation, and alert drift (like any other
    field) heals with one edit."""

    def setUp(self) -> None:
        self.job = stored_job({"kind": "every", "everyMs": 900000})
        oc_patcher = mock.patch.object(seed_automations, "openclaw")
        self.oc = oc_patcher.start()
        self.oc.return_value = CompletedProcess(args=[], returncode=0, stdout="", stderr="")
        self.addCleanup(oc_patcher.stop)
        list_patcher = mock.patch.object(
            seed_automations, "list_cron_jobs", return_value=[self.job]
        )
        list_patcher.start()
        self.addCleanup(list_patcher.stop)

    def test_flags_empty_without_chat(self) -> None:
        with mock.patch.object(seed_automations, "CHAT", ""):
            self.assertEqual([], seed_automations.failure_alert_flags())

    def test_flags_route_alerts_to_chat(self) -> None:
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            self.assertEqual(
                [
                    "--failure-alert",
                    "--failure-alert-channel",
                    "telegram",
                    "--failure-alert-to",
                    "-100",
                    "--failure-alert-include-skipped",
                ],
                seed_automations.failure_alert_flags(),
            )

    def test_new_job_gets_alerts_attached_via_edit(self) -> None:
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            seed_automations.reconcile(make_spec(), [], "m")
        add_call = self.oc.call_args_list[0][0][0]
        edit_call = self.oc.call_args_list[1][0][0]
        self.assertEqual(["cron", "add"], add_call[:2])
        self.assertEqual(["cron", "edit", "job-1"], edit_call[:3])
        self.assertIn("--failure-alert", edit_call)
        self.assertIn("-100", edit_call)

    def test_matching_job_without_alerts_is_drift_and_edited(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000})
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            self.assertFalse(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_matching_job_with_alerts_is_current(self) -> None:
        job = stored_job(
            {"kind": "every", "everyMs": 900000},
            delivery={"to": "-100"},
            failureAlert={"enabled": True, "channel": "telegram", "to": "-100"},
        )
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            self.assertTrue(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_alerts_ignored_when_chat_unset(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000})
        with mock.patch.object(seed_automations, "CHAT", ""):
            self.assertTrue(seed_automations._job_is_current(job, make_spec(), "m"))


class ScheduleValidation(TempSpecDir):
    """Fail-closed schedule syntax: an invalid every-duration or cron
    expression used to parse fine, then never match stored state — firing
    `cron edit` on every boot forever."""

    def test_invalid_every_durations_rejected(self) -> None:
        for bad in ("15x", "1h30", "1.5", "0", "0s", "-5m", "m", "1.5.5m"):
            with self.subTest(every=bad):
                self.write("test-job.md", VALID_SPEC.replace("every: 15m", f"every: {bad}"))
                with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
                    self.load()
                self.assertIn("every", str(ctx.exception))

    def test_valid_every_durations_accepted(self) -> None:
        for good in ("15m", "1h30m", "0.25h", "500", "1w", "0.5d"):
            with self.subTest(every=good):
                self.write("test-job.md", VALID_SPEC.replace("every: 15m", f"every: {good}"))
                specs = self.load()
                self.assertEqual(good, specs[0].schedule_value)

    def test_invalid_cron_expressions_rejected(self) -> None:
        for bad in (
            "* * * *",  # 4 fields
            "* * * * * *",  # 6 fields
            "x * * * *",  # non-field token
            "*/ * * * *",  # step without value
            "61- * * * *",  # malformed range
            "* * * * 61x",  # trailing garbage
            "mon * * *",  # name + wrong arity
        ):
            with self.subTest(cron=bad):
                self.write("test-job.md", VALID_SPEC.replace("every: 15m", f"cron: {bad}"))
                with self.assertRaises(seed_automations.AutomationSpecError) as ctx:
                    self.load()
                self.assertIn("cron", str(ctx.exception))

    def test_valid_cron_expressions_accepted(self) -> None:
        for good in ("2 9 * * *", "*/5 * * * *", "0 9 * * mon-fri", "30 4 1,15 * *"):
            with self.subTest(cron=good):
                self.write("test-job.md", VALID_SPEC.replace("every: 15m", f"cron: {good}"))
                specs = self.load()
                self.assertEqual("--cron", specs[0].schedule_flag)
                self.assertEqual(good, specs[0].schedule_value)


class CliTimeout(unittest.TestCase):
    """Every openclaw() spawn carries a timeout — the reconciler runs in
    the forked post-startup child, where a hung CLI hangs it forever."""

    def test_openclaw_passes_timeout(self) -> None:
        captured: dict[str, object] = {}

        def fake_run(cmd: object, **kwargs: object) -> subprocess.CompletedProcess[str]:
            captured.update(kwargs)
            return subprocess.CompletedProcess(cmd, 0, stdout="", stderr="")

        with mock.patch.object(seed_automations.subprocess, "run", side_effect=fake_run):
            seed_automations.openclaw(["cron", "list", "--json"])
        self.assertGreaterEqual(int(captured.get("timeout", 0)), 60)

    def test_timeout_returns_synthetic_failure_not_exception(self) -> None:
        def hanging(cmd: object, **kwargs: object) -> subprocess.CompletedProcess[str]:
            raise subprocess.TimeoutExpired(cmd, 60)

        with mock.patch.object(seed_automations.subprocess, "run", side_effect=hanging):
            result = seed_automations.openclaw(["cron", "list", "--json"])
        self.assertEqual(124, result.returncode)
        self.assertIn("timed out", result.stderr)


class DurationParsing(unittest.TestCase):
    def test_parses_units_composites_and_bare_ms(self) -> None:
        cases: list[tuple[str, int]] = [
            ("15m", 900000),
            ("45s", 45000),
            ("1h", 3600000),
            ("2d", 172800000),
            ("1w", 604800000),
            ("1h30m", 5400000),
            ("0.25h", 900000),
            ("900000", 900000),
            ("250ms", 250),
        ]
        for value, expected in cases:
            with self.subTest(value=value):
                self.assertEqual(seed_automations._parse_duration_ms(value), expected)

    def test_rejects_unparseable_values(self) -> None:
        for value in ("15x", "", "m", "1.5.2h", "every 15m", "15 m"):
            with self.subTest(value=value):
                self.assertIsNone(seed_automations._parse_duration_ms(value))


class ScheduleCurrent(unittest.TestCase):
    """Precise kind-dict comparison: kind, everyMs as a number (never the
    string form, never a bool), cron expr whitespace-normalized — while
    ignoring server-added anchorMs and policy staggerMs/tz. Unreadable
    stored schedules are drift."""

    def setUp(self) -> None:
        patcher = mock.patch.object(seed_automations, "CHAT", "")
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_every_job_matching_every_ms_is_current(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000, "anchorMs": 1755734400000})
        self.assertTrue(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_every_job_drifted_every_ms_is_drift(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 600000})
        self.assertFalse(seed_automations._job_is_current(job, make_spec(value="15m"), "m"))

    def test_every_spec_vs_stored_cron_kind_is_drift(self) -> None:
        job = stored_job({"kind": "cron", "expr": "2 9 * * *"})
        self.assertFalse(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_cron_spec_vs_stored_every_kind_is_drift(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000})
        spec = make_spec(flag="--cron", value="2 9 * * *")
        self.assertFalse(seed_automations._job_is_current(job, spec, "m"))

    def test_cron_job_matching_expr_is_current(self) -> None:
        schedule = {
            "kind": "cron",
            "expr": "2 9 * * *",
            "tz": "America/New_York",
            "staggerMs": 30000,
        }
        spec = make_spec(flag="--cron", value="2 9 * * *")
        self.assertTrue(seed_automations._job_is_current(stored_job(schedule), spec, "m"))

    def test_cron_expr_whitespace_variants_match(self) -> None:
        job = stored_job({"kind": "cron", "expr": "2  9 *   * *"})
        spec = make_spec(flag="--cron", value="2 9 * * *")
        self.assertTrue(seed_automations._job_is_current(job, spec, "m"))

    def test_cron_expr_drift_is_drift(self) -> None:
        job = stored_job({"kind": "cron", "expr": "3 9 * * *"})
        spec = make_spec(flag="--cron", value="2 9 * * *")
        self.assertFalse(seed_automations._job_is_current(job, spec, "m"))

    def test_unreadable_schedules_are_drift(self) -> None:
        for schedule in (
            None,
            "every 15m",
            {},
            {"kind": "every"},
            {"kind": "every", "everyMs": "900000"},
            {"kind": "every", "everyMs": True},
            {"kind": "cron"},
            {"kind": "cron", "expr": 42},
        ):
            with self.subTest(schedule=schedule):
                self.assertFalse(
                    seed_automations._job_is_current(stored_job(schedule), make_spec(), "m")
                )

    def test_unparseable_spec_duration_fails_open_to_drift(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000})
        self.assertFalse(seed_automations._job_is_current(job, make_spec(value="15x"), "m"))


class TolerantStoredShapes(unittest.TestCase):
    """The legacy fallback: a plain-string schedule, or a dict carrying
    expression/cron/every/value string keys, compares whitespace-collapsed
    against the literal spec value — plus nothing else."""

    def spec(self, flag: str = "--every", value: str = "15m") -> seed_automations.JobSpec:
        return make_spec(flag=flag, value=value)

    def test_stored_schedule_string_reader(self) -> None:
        self.assertEqual(
            seed_automations._stored_schedule_string({"schedule": "*/5  * * * *"}),
            "*/5 * * * *",
        )
        self.assertEqual(
            seed_automations._stored_schedule_string({"schedule": {"every": "15m"}}),
            "15m",
        )
        self.assertIsNone(seed_automations._stored_schedule_string({"schedule": ""}))
        self.assertIsNone(seed_automations._stored_schedule_string({}))

    def test_plain_string_matches_literal(self) -> None:
        job = {"schedule": "15m"}
        self.assertTrue(seed_automations._schedule_is_current(job, self.spec()))

    def test_plain_string_cron_collapses_whitespace(self) -> None:
        job = {"schedule": "2  9 *  * *"}
        spec = self.spec(flag="--cron", value="2 9 * * *")
        self.assertTrue(seed_automations._schedule_is_current(job, spec))

    def test_legacy_dict_keys_match_literal(self) -> None:
        for key in ("expression", "cron", "every", "value"):
            with self.subTest(key=key):
                job = {"schedule": {key: "15m"}}
                self.assertTrue(seed_automations._schedule_is_current(job, self.spec()))

    def test_cron_spec_matches_legacy_dict(self) -> None:
        job = {"schedule": {"expression": "0 9 * * *"}}
        spec = self.spec(flag="--cron", value="0 9 * * *")
        self.assertTrue(seed_automations._schedule_is_current(job, spec))

    def test_blank_or_opaque_shapes_are_drift(self) -> None:
        for schedule in ("", "   ", {"opaque": 1}, {}, {"expression": 42}):
            with self.subTest(schedule=schedule):
                self.assertFalse(
                    seed_automations._schedule_is_current({"schedule": schedule}, self.spec())
                )

    def test_literal_only_never_translates_forms(self) -> None:
        # Anti-false-match lock: no every→cron conversion is used as a
        # match candidate — a stored cron-form for an every-spec is drift.
        schedules: list[object] = [
            "*/15 * * * *",
            {"expression": "*/15 * * * *"},
            {"value": "0 */15 * * *"},
        ]
        for schedule in schedules:
            with self.subTest(schedule=schedule):
                self.assertFalse(
                    seed_automations._schedule_is_current({"schedule": schedule}, self.spec())
                )

    def test_kind_dict_takes_precedence_over_legacy_keys(self) -> None:
        # A dict carrying 'kind' is the CLI contract shape: compared
        # precisely via 'expr', never through the legacy fallback.
        job = {"schedule": {"kind": "cron", "expression": "2 9 * * *"}}
        spec = self.spec(flag="--cron", value="2 9 * * *")
        self.assertFalse(seed_automations._schedule_is_current(job, spec))


class JobCurrency(unittest.TestCase):
    """_job_is_current requires message, model, threadId (when the spec
    has a topic), delivery.to (when CHAT is set), and schedule to all
    match."""

    def setUp(self) -> None:
        patcher = mock.patch.object(seed_automations, "CHAT", "")
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_fully_matching_job_is_current(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000})
        self.assertTrue(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_message_drift_is_not_current(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, message="old prompt")
        self.assertFalse(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_thread_drift_is_not_current_when_spec_has_topic(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, delivery={"threadId": "41"})
        self.assertFalse(seed_automations._job_is_current(job, make_spec(topic="42"), "m"))

    def test_thread_ignored_when_spec_has_no_topic(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, delivery={"threadId": "41"})
        self.assertTrue(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_chat_drift_is_not_current_when_chat_set(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, delivery={"to": "-111"})
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            self.assertFalse(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_chat_ignored_when_chat_unset(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, delivery={"to": "-111"})
        self.assertTrue(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_model_drift_is_not_current(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, model="old/model")
        self.assertFalse(seed_automations._job_is_current(job, make_spec(), "m"))

    def test_job_model_wins_over_global_for_currency(self) -> None:
        job = stored_job({"kind": "every", "everyMs": 900000}, model="zai/mini")
        self.assertTrue(
            seed_automations._job_is_current(job, make_spec(model="zai/mini"), "global/model")
        )


class DeliveryFlags(unittest.TestCase):
    def test_no_chat_yields_no_flags(self) -> None:
        with mock.patch.object(seed_automations, "CHAT", ""):
            self.assertEqual(seed_automations.delivery_flags(make_spec()), [])

    def test_announce_with_thread(self) -> None:
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            self.assertEqual(
                seed_automations.delivery_flags(make_spec(topic="42")),
                [
                    "--announce",
                    "--channel",
                    "telegram",
                    "--to",
                    "-100",
                    "--thread-id",
                    "42",
                ],
            )

    def test_no_deliver_mode(self) -> None:
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            self.assertEqual(
                seed_automations.delivery_flags(make_spec(deliver="no-deliver")),
                ["--no-deliver", "--channel", "telegram", "--to", "-100"],
            )


class ReconcileContract(unittest.TestCase):
    """reconcile() against a mocked CLI — no real openclaw binary. The
    resolved model threads through to `cron add`; a drifted job re-asserts
    via `cron edit` with the schedule flags included."""

    def setUp(self) -> None:
        self.chat_patcher = mock.patch.object(seed_automations, "CHAT", "")
        self.chat_patcher.start()
        self.addCleanup(self.chat_patcher.stop)
        self.oc_patcher = mock.patch.object(seed_automations, "openclaw")
        self.oc = self.oc_patcher.start()
        self.addCleanup(self.oc_patcher.stop)
        self.oc.return_value = CompletedProcess(args=[], returncode=0, stdout="", stderr="")

    def test_missing_job_created_with_model_and_delivery(self) -> None:
        spec = make_spec(topic="42")
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            seed_automations.reconcile(spec, [], "zai/test-model")
        self.assertEqual(2, self.oc.call_count)
        self.oc.assert_any_call(
            [
                "cron",
                "add",
                "--every",
                "15m",
                "--message",
                "do things",
                "--name",
                "test-job",
                "--agent",
                "main",
                "--session",
                "isolated",
                "--model",
                "zai/test-model",
                "--tools",
                ",".join(seed_automations.DEFAULT_JOB_TOOLS),
                "--announce",
                "--channel",
                "telegram",
                "--to",
                "-100",
                "--thread-id",
                "42",
            ]
        )
        self.oc.assert_any_call(["cron", "list", "--json"])

    def test_create_without_chat_omits_delivery_flags(self) -> None:
        seed_automations.reconcile(make_spec(deliver="no-deliver"), [], "m")
        call = self.oc.call_args[0][0]
        self.assertNotIn("--no-deliver", call)
        self.assertNotIn("--announce", call)
        self.assertNotIn("--to", call)

    def test_current_job_skipped_without_cli_call(self) -> None:
        spec = make_spec()
        job = stored_job({"kind": "every", "everyMs": 900000})
        seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_not_called()

    def test_schedule_drift_edits_with_every_flag(self) -> None:
        spec = make_spec(value="10m")
        job = stored_job({"kind": "every", "everyMs": 900000})
        seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_called_once_with(
            ["cron", "edit", "job-1", "--message", "do things", "--every", "10m", "--model", "m"]
            + ["--tools", ",".join(seed_automations.DEFAULT_JOB_TOOLS)]
        )

    def test_cron_schedule_drift_edits_with_cron_flag(self) -> None:
        spec = make_spec(flag="--cron", value="2 9 * * *")
        job = stored_job({"kind": "cron", "expr": "3 9 * * *"})
        seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_called_once_with(
            [
                "cron",
                "edit",
                "job-1",
                "--message",
                "do things",
                "--cron",
                "2 9 * * *",
                "--model",
                "m",
            ]
            + ["--tools", ",".join(seed_automations.DEFAULT_JOB_TOOLS)]
        )

    def test_stored_cron_form_for_every_spec_edits(self) -> None:
        # Anti-false-match lock: the every→cron conversion was NOT
        # adopted as a match candidate — this heals via one idempotent
        # edit that re-asserts the every form.
        spec = make_spec()
        job = stored_job({"kind": "cron", "expr": "*/15 * * * *"})
        seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_called_once_with(
            ["cron", "edit", "job-1", "--message", "do things", "--every", "15m", "--model", "m"]
            + ["--tools", ",".join(seed_automations.DEFAULT_JOB_TOOLS)]
        )

    def test_message_drift_edits_with_schedule_flags(self) -> None:
        spec = make_spec()
        job = stored_job({"kind": "every", "everyMs": 900000}, message="old prompt")
        seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_called_once_with(
            ["cron", "edit", "job-1", "--message", "do things", "--every", "15m", "--model", "m"]
            + ["--tools", ",".join(seed_automations.DEFAULT_JOB_TOOLS)]
        )

    def test_job_model_drift_edits_with_job_model(self) -> None:
        spec = make_spec(model="zai/mini")
        job = stored_job({"kind": "every", "everyMs": 900000}, model="old/model")
        seed_automations.reconcile(spec, [job], "global/model")
        self.oc.assert_called_once_with(
            [
                "cron",
                "edit",
                "job-1",
                "--message",
                "do things",
                "--every",
                "15m",
                "--model",
                "zai/mini",
            ]
            + ["--tools", ",".join(seed_automations.DEFAULT_JOB_TOOLS)]
        )

    def test_global_model_drift_edits_with_global_model(self) -> None:
        spec = make_spec()
        job = stored_job({"kind": "every", "everyMs": 900000}, model="old/model")
        seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_called_once_with(
            ["cron", "edit", "job-1", "--message", "do things", "--every", "15m", "--model", "m"]
            + ["--tools", ",".join(seed_automations.DEFAULT_JOB_TOOLS)]
        )

    def test_matching_job_model_skips_edit(self) -> None:
        spec = make_spec(model="zai/mini")
        job = stored_job({"kind": "every", "everyMs": 900000}, model="zai/mini")
        seed_automations.reconcile(spec, [job], "global/model")
        self.oc.assert_not_called()

    def test_chat_drift_edits_with_chat_flags(self) -> None:
        spec = make_spec()
        job = stored_job({"kind": "every", "everyMs": 900000}, delivery={"to": "-100"})
        with mock.patch.object(seed_automations, "CHAT", "-999"):
            seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_called_once_with(
            [
                "cron",
                "edit",
                "job-1",
                "--message",
                "do things",
                "--every",
                "15m",
                "--model",
                "m",
                "--tools",
                ",".join(seed_automations.DEFAULT_JOB_TOOLS),
                "--announce",
                "--channel",
                "telegram",
                "--to",
                "-999",
                "--failure-alert",
                "--failure-alert-channel",
                "telegram",
                "--failure-alert-to",
                "-999",
                "--failure-alert-include-skipped",
            ]
        )

    def test_thread_drift_edits_with_resolved_thread(self) -> None:
        spec = make_spec(topic="42")
        job = stored_job({"kind": "every", "everyMs": 900000}, delivery={"threadId": "41"})
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            seed_automations.reconcile(spec, [job], "m")
        self.oc.assert_called_once_with(
            [
                "cron",
                "edit",
                "job-1",
                "--message",
                "do things",
                "--every",
                "15m",
                "--model",
                "m",
                "--tools",
                ",".join(seed_automations.DEFAULT_JOB_TOOLS),
                "--announce",
                "--channel",
                "telegram",
                "--to",
                "-100",
                "--thread-id",
                "42",
                "--failure-alert",
                "--failure-alert-channel",
                "telegram",
                "--failure-alert-to",
                "-100",
                "--failure-alert-include-skipped",
            ]
        )

    def test_prunes_duplicates_keeping_chat_match(self) -> None:
        spec = make_spec()
        stray = stored_job(
            {"kind": "every", "everyMs": 900000},
            id="job-stray",
            delivery={"to": "-111"},
        )
        keeper = stored_job(
            {"kind": "every", "everyMs": 900000},
            id="job-keep",
            delivery={"to": "-100"},
            failureAlert={"enabled": True, "channel": "telegram", "to": "-100"},
        )
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            seed_automations.reconcile(spec, [stray, keeper], "m")
        self.oc.assert_called_once_with(["cron", "delete", "job-stray"])

    def test_failed_duplicate_delete_warns_without_raising(self) -> None:
        spec = make_spec()
        stray = stored_job(
            {"kind": "every", "everyMs": 900000},
            id="job-stray",
            delivery={"to": "-111"},
        )
        keeper = stored_job(
            {"kind": "every", "everyMs": 900000},
            id="job-keep",
            delivery={"to": "-100"},
            failureAlert={"enabled": True, "channel": "telegram", "to": "-100"},
        )
        self.oc.return_value = CompletedProcess(args=[], returncode=1, stdout="", stderr="boom")
        with mock.patch.object(seed_automations, "CHAT", "-100"):
            seed_automations.reconcile(spec, [stray, keeper], "m")  # must not raise
        self.assertEqual(self.oc.call_count, 1)
        self.assertEqual(self.oc.call_args[0][0][:3], ["cron", "delete", "job-stray"])

    def test_failed_edit_warns_without_raising(self) -> None:
        spec = make_spec(value="10m")
        job = stored_job({"kind": "every", "everyMs": 900000})
        self.oc.return_value = CompletedProcess(args=[], returncode=1, stdout="", stderr="boom")
        seed_automations.reconcile(spec, [job], "m")  # must not raise
        self.assertEqual(self.oc.call_args[0][0][:3], ["cron", "edit", "job-1"])


class MainFlow(unittest.TestCase):
    """Fail-closed no-mutation proofs and the one-listing-per-run rule."""

    def setUp(self) -> None:
        self.chat_patcher = mock.patch.object(seed_automations, "CHAT", "")
        self.chat_patcher.start()
        self.addCleanup(self.chat_patcher.stop)

    def test_single_cron_list_per_run(self) -> None:
        spec = make_spec()
        job = stored_job({"kind": "every", "everyMs": 900000}, model="zai/test-model")
        with (
            mock.patch.object(seed_automations, "build_jobs", return_value=[spec]),
            mock.patch.object(seed_automations, "list_cron_jobs", return_value=[job]) as lst,
            mock.patch.object(seed_automations, "openclaw") as oc,
        ):
            seed_automations.main(["--model", "zai/test-model"])
        self.assertEqual(lst.call_count, 1)
        oc.assert_not_called()

    def test_job_model_flows_into_cron_add(self) -> None:
        spec = make_spec(model="zai/mini")
        with (
            mock.patch.object(seed_automations, "build_jobs", return_value=[spec]),
            mock.patch.object(seed_automations, "list_cron_jobs", return_value=[]),
            mock.patch.object(seed_automations, "openclaw") as oc,
        ):
            oc.return_value = CompletedProcess(args=[], returncode=0, stdout="", stderr="")
            seed_automations.main(["--model", "global/model"])
        oc.assert_called_once()
        add_argv = oc.call_args[0][0]
        self.assertEqual(["cron", "add"], add_argv[:2])
        self.assertEqual("zai/mini", add_argv[add_argv.index("--model") + 1])

    def test_cli_model_wins_and_flows_into_cron_add(self) -> None:
        spec = make_spec()
        with (
            mock.patch.dict(os.environ, {"AUTOMATION_MODEL": "env/model"}),
            mock.patch.object(seed_automations, "build_jobs", return_value=[spec]),
            mock.patch.object(seed_automations, "list_cron_jobs", return_value=[]),
            mock.patch.object(seed_automations, "openclaw") as oc,
        ):
            oc.return_value = CompletedProcess(args=[], returncode=0, stdout="", stderr="")
            seed_automations.main(["--model", "cli/model"])
        oc.assert_called_once_with(
            [
                "cron",
                "add",
                "--every",
                "15m",
                "--message",
                "do things",
                "--name",
                "test-job",
                "--agent",
                "main",
                "--session",
                "isolated",
                "--model",
                "cli/model",
                "--tools",
                ",".join(seed_automations.DEFAULT_JOB_TOOLS),
            ]
        )

    def test_missing_model_aborts_naming_both_sources(self) -> None:
        spec = make_spec()
        stderr = io.StringIO()
        with (
            mock.patch.dict(os.environ),
            mock.patch.object(seed_automations, "build_jobs", return_value=[spec]),
            mock.patch.object(seed_automations, "list_cron_jobs") as lst,
            mock.patch.object(seed_automations, "openclaw") as oc,
            contextlib.redirect_stderr(stderr),
        ):
            os.environ.pop("AUTOMATION_MODEL", None)
            with self.assertRaises(SystemExit) as ctx:
                seed_automations.main([])
        self.assertEqual(ctx.exception.code, 1)
        output = stderr.getvalue()
        self.assertIn("--model", output)
        self.assertIn("AUTOMATION_MODEL", output)
        lst.assert_not_called()
        oc.assert_not_called()

    def test_spec_error_aborts_before_listing(self) -> None:
        with (
            mock.patch.object(
                seed_automations,
                "build_jobs",
                side_effect=seed_automations.AutomationSpecError("broken"),
            ),
            mock.patch.object(seed_automations, "list_cron_jobs") as lst,
            mock.patch.object(seed_automations, "openclaw") as oc,
            self.assertRaises(SystemExit) as ctx,
        ):
            seed_automations.main(["--model", "zai/test-model"])
        self.assertEqual(ctx.exception.code, 1)
        lst.assert_not_called()
        oc.assert_not_called()

    def test_unparseable_listing_aborts_without_touching_crons(self) -> None:
        spec = make_spec()
        with (
            mock.patch.object(seed_automations, "build_jobs", return_value=[spec]),
            mock.patch.object(seed_automations, "list_cron_jobs", return_value=None),
            mock.patch.object(seed_automations, "openclaw") as oc,
            self.assertRaises(SystemExit) as ctx,
        ):
            seed_automations.main(["--model", "zai/test-model"])
        self.assertEqual(ctx.exception.code, 1)
        oc.assert_not_called()


if __name__ == "__main__":
    unittest.main()
