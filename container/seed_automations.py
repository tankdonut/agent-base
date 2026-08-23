#!/usr/bin/env python3
# allow: SIZE_OK — contractually a single-file module (exactly two files
# land in this repo); a cohesive idempotent reconciler ported verbatim in
# structure from the freya/mimir originals.
"""Reconcile scheduled OpenClaw cron jobs from markdown specs (idempotent).

Job specs live as markdown files in the automations directory next to this
script (image-baked via the Dockerfile — never host-mounted; writable
prompt files would be a self-modification surface for the agent). Each
file is a flat ``key: value`` header fenced by ``---`` lines plus a prompt
body; ``{{JOURNAL}}``/``{{DOCS}}`` tokens resolve to the live workspace
paths at load time. The loader parses strictly and fails closed: any
schema violation, unknown key/token, duplicate key or name, empty
directory, or unparseable file aborts the whole run without creating,
editing, or pruning any job — a missing spec must never silently prune or
freeze the live crons.

Bodies render to a single canonical line (all whitespace runs collapse to
one space), so markdown reformatting hooks can never churn the deployed
message string and trigger needless ``cron edit`` calls.

Standard agent contract (union of the freya and mimir originals):

  AGENT_AUTOMATIONS_DIR  Override the automations directory (default:
                         <script dir>/automations).
  TELEGRAM_CHAT_ID       Chat ID for delivery (optional; if unset, jobs
                         run without Telegram delivery).
  TELEGRAM_TOPIC_*       Telegram forum topic IDs per job (via topic-env).
  --model / AUTOMATION_MODEL
                         Model for cron agent turns. There is deliberately
                         no baked default — a default would silently drift
                         models between agents sharing this image. --model
                         (the entrypoint passes spec.automations.model)
                         wins over AUTOMATION_MODEL; when neither is set
                         the run aborts before any mutation.

Schedule comparison: a stored kind-dict (``{'kind': 'every', 'everyMs':
<int>}`` or ``{'kind': 'cron', 'expr': <str>}``) is compared precisely —
everyMs against the parsed duration (compound forms like ``1h30m``
included), cron exprs whitespace-collapsed. Any other recognizable shape
— schedule stored as a plain string, or a dict carrying
``expression``/``cron``/``every``/``value`` string keys (legacy or foreign
storage) — compares whitespace-collapsed against the literal spec value
and nothing else. A stored cron-form for an every-spec is therefore
DRIFT, never a match: it heals via exactly one idempotent ``cron edit``
per job on the first boot of a project migrating from cron-form storage,
then stays stable.

Run on every container boot via the agent entrypoint.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

CHAT = os.environ.get("TELEGRAM_CHAT_ID", "")

WORKSPACE = Path.home() / ".openclaw" / "workspace"
DOCS = WORKSPACE / "docs"
JOURNAL = WORKSPACE / "journal"

TOKENS = {"JOURNAL": str(JOURNAL), "DOCS": str(DOCS)}
TOKEN_RE = re.compile(r"\{\{\s*([A-Z][A-Z_]*)\s*\}\}")
NAME_RE = re.compile(r"[a-z0-9][a-z0-9-]*")
ENV_NAME_RE = re.compile(r"[A-Z][A-Z0-9_]*")

HEADER_KEYS = frozenset({"name", "every", "cron", "deliver", "topic-env"})
DELIVER_MODES = frozenset({"announce", "no-deliver"})

DURATION_TOKEN_RE = re.compile(r"(\d+(?:\.\d+)?)(ms|s|m|h|d|w)")
DURATION_UNITS_MS: dict[str, float] = {
    "ms": 1,
    "s": 1000,
    "m": 60000,
    "h": 3600000,
    "d": 86400000,
    "w": 604800000,
}

CRON_NAME = r"(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec|mon|tue|wed|thu|fri|sat|sun)"
CRON_PART_RE = re.compile(
    rf"(?:\*|\d{{1,4}}|{CRON_NAME})(?:-(?:\d{{1,4}}|{CRON_NAME}))?(?:/\d{{1,4}})?"
)


class AutomationSpecError(ValueError):
    """A markdown automation spec failed strict validation."""


class ModelResolutionError(RuntimeError):
    """No automation model was configured via --model or AUTOMATION_MODEL."""


@dataclass(frozen=True)
class JobSpec:
    name: str
    schedule_flag: str
    schedule_value: str
    prompt: str
    topic: str
    deliver: str


def resolve_model(cli_model: str | None) -> str:
    """Resolve the cron-agent model: --model beats AUTOMATION_MODEL.

    There is deliberately no baked default — a default would silently
    drift models between agents sharing this base image, so an empty
    resolution is a hard error naming both sources."""
    model = (cli_model or "").strip() or os.environ.get("AUTOMATION_MODEL", "").strip()
    if not model:
        raise ModelResolutionError(
            "no automation model configured — pass --model or set AUTOMATION_MODEL"
        )
    return model


def _parse_header(text: str, path: Path) -> tuple[dict[str, str], str]:
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        raise AutomationSpecError(f"{path.name}: must open with a '---' fence")
    header: dict[str, str] = {}
    for idx in range(1, len(lines)):
        line = lines[idx]
        if line.strip() == "---":
            return header, "\n".join(lines[idx + 1 :])
        key, sep, value = line.partition(":")
        if not sep:
            raise AutomationSpecError(f"{path.name}:{idx + 1}: expected 'key: value', got {line!r}")
        key = key.strip()
        if key not in HEADER_KEYS:
            raise AutomationSpecError(
                f"{path.name}:{idx + 1}: unknown header key {key!r} "
                f"(allowed: {sorted(HEADER_KEYS)})"
            )
        if key in header:
            raise AutomationSpecError(f"{path.name}:{idx + 1}: duplicate header key {key!r}")
        header[key] = value.strip()
    raise AutomationSpecError(f"{path.name}: header fence never closed")


def _render_prompt(body: str, path: Path) -> str:
    def substitute(match: re.Match[str]) -> str:
        token = match.group(1)
        if token not in TOKENS:
            raise AutomationSpecError(
                f"{path.name}: unknown token {match.group(0)!r} (allowed: {sorted(TOKENS)})"
            )
        return TOKENS[token]

    rendered = TOKEN_RE.sub(substitute, body)
    if "{{" in rendered:
        raise AutomationSpecError(f"{path.name}: unrecognized token syntax in prompt body")
    return " ".join(rendered.split())


def _load_spec(path: Path) -> JobSpec:
    header, body = _parse_header(path.read_text(encoding="utf-8"), path)

    name = header.get("name", "")
    if not NAME_RE.fullmatch(name):
        raise AutomationSpecError(
            f"{path.name}: invalid or missing name {name!r} (lowercase kebab-case required)"
        )
    if name != path.stem:
        raise AutomationSpecError(f"{path.name}: name {name!r} must match file stem {path.stem!r}")

    has_every, has_cron = "every" in header, "cron" in header
    if has_every == has_cron:
        raise AutomationSpecError(f"{path.name}: exactly one of 'every' or 'cron' is required")
    schedule_flag = "--every" if has_every else "--cron"
    schedule_value = header["every" if has_every else "cron"]
    if not schedule_value:
        raise AutomationSpecError(f"{path.name}: schedule value is empty")
    if has_every:
        duration_ms = _parse_duration_ms(schedule_value)
        if duration_ms is None or duration_ms <= 0:
            raise AutomationSpecError(
                f"{path.name}: invalid 'every' duration {schedule_value!r} "
                "(expected e.g. 15m, 1h30m, 0.25h, or bare milliseconds)"
            )
    elif not _valid_cron_expression(schedule_value):
        raise AutomationSpecError(
            f"{path.name}: invalid 'cron' expression {schedule_value!r} "
            "(expected 5 fields: minute hour day-of-month month day-of-week)"
        )

    deliver = header.get("deliver", "")
    if deliver not in DELIVER_MODES:
        raise AutomationSpecError(
            f"{path.name}: deliver must be one of {sorted(DELIVER_MODES)}, got {deliver!r}"
        )

    topic_env = header.get("topic-env", "")
    if topic_env and not ENV_NAME_RE.fullmatch(topic_env):
        raise AutomationSpecError(f"{path.name}: invalid topic-env {topic_env!r}")
    topic = os.environ.get(topic_env, "") if topic_env else ""

    prompt = _render_prompt(body, path)
    if not prompt:
        raise AutomationSpecError(f"{path.name}: prompt body is empty")

    return JobSpec(
        name=name,
        schedule_flag=schedule_flag,
        schedule_value=schedule_value,
        prompt=prompt,
        topic=topic,
        deliver=deliver,
    )


def build_jobs(directory: Path | None = None) -> list[JobSpec]:
    """Load JobSpecs from markdown files; raise AutomationSpecError on any
    validation problem (fail-closed — callers must not reconcile stale
    live jobs against a partial spec set)."""
    if directory is None:
        env_dir = os.environ.get("AGENT_AUTOMATIONS_DIR", "")
        directory = Path(env_dir) if env_dir else Path(__file__).resolve().parent / "automations"
    if not directory.is_dir():
        raise AutomationSpecError(f"automations directory not found: {directory}")
    files = sorted(directory.glob("*.md"))
    if not files:
        raise AutomationSpecError(f"no automation specs found in {directory}")
    specs = [_load_spec(path) for path in files]
    names = [spec.name for spec in specs]
    duplicates = {n for n in names if names.count(n) > 1}
    if duplicates:
        raise AutomationSpecError(f"duplicate job names: {sorted(duplicates)}")
    return specs


def openclaw(args: list[str]) -> subprocess.CompletedProcess[str]:
    """Spawn the openclaw CLI with a hard timeout.

    The reconciler runs in the forked post-startup child, where a hung CLI
    would hang the child forever. 60s covers every cron subcommand with
    wide margin (memory index, the slow one, is the entrypoint's own call
    with its own 600s budget). A timeout returns a synthetic failure
    (exit 124, stderr names the timeout) so existing returncode paths warn
    uniformly instead of raising out of the child."""
    try:
        return subprocess.run(
            ["openclaw", *args],
            capture_output=True,
            text=True,
            timeout=60,
        )
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(
            ["openclaw", *args], 124, stdout="", stderr="openclaw timed out after 60s"
        )


def list_cron_jobs() -> list[dict] | None:
    """Return the current cron job list, or None if it cannot be parsed."""
    result = openclaw(["cron", "list", "--json"])
    if result.returncode != 0:
        return None
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError:
        return None
    if isinstance(data, dict):
        return data.get("jobs") or data.get("data") or []
    if isinstance(data, list):
        return data
    return None


def delivery_flags(spec: JobSpec) -> list[str]:
    """Telegram delivery flags: none without a chat; otherwise the
    announce mode, channel target, and the resolved topic thread."""
    if not CHAT:
        return []
    mode = "--no-deliver" if spec.deliver == "no-deliver" else "--announce"
    flags = [mode, "--channel", "telegram", "--to", CHAT]
    if spec.topic:
        flags.extend(["--thread-id", spec.topic])
    return flags


def _valid_cron_expression(value: str) -> bool:
    """Shape check for a 5-field vixie-cron expression: fields of
    comma-separated parts, each `*`, a number or 3-letter name, optionally
    a range and/or step. Field VALUE ranges are the CLI's business — a
    value it rejects fails `cron add` loudly (warn), not silently."""
    fields = value.split()
    if len(fields) != 5:
        return False
    for field in fields:
        parts = field.split(",")
        if not parts or not all(CRON_PART_RE.fullmatch(part) for part in parts):
            return False
    return True


def _parse_duration_ms(value: str) -> int | None:
    """Parse a duration like '15m', '1h30m', '0.25h', or bare milliseconds
    into integer ms — mirroring how the CLI's `--every` is stored as
    `everyMs`. Returns None when the value is not a positive duration."""
    if value.isdigit():
        return int(value)
    total = 0.0
    pos = 0
    for match in DURATION_TOKEN_RE.finditer(value):
        if match.start() != pos:
            return None
        total += float(match.group(1)) * DURATION_UNITS_MS[match.group(2)]
        pos = match.end()
    if pos != len(value) or pos == 0:
        return None
    return int(total)


def _collapse_ws(value: str) -> str:
    return " ".join(value.split())


def _stored_schedule_string(job: dict) -> str | None:
    """Legacy/foreign stored shapes as a comparable string: a plain-string
    schedule, or a dict carrying expression/cron/every/value string keys.
    Returns None when nothing recognizable (→ drift)."""
    schedule = job.get("schedule")
    if isinstance(schedule, str) and schedule.strip():
        return _collapse_ws(schedule)
    if isinstance(schedule, dict):
        for key in ("expression", "cron", "every", "value"):
            value = schedule.get(key)
            if isinstance(value, str) and value.strip():
                return _collapse_ws(value)
    return None


def _kind_schedule_is_current(stored: dict, spec: JobSpec) -> bool:
    """Precise comparison for the cron CLI's contract shapes: {'kind':
    'every', 'everyMs': <int>} or {'kind': 'cron', 'expr': <str>}.
    anchorMs is server-added phase, staggerMs/tz are policy — ignored."""
    if spec.schedule_flag == "--every":
        if stored.get("kind") != "every":
            return False
        every_ms = stored.get("everyMs")
        # bool is excluded explicitly: True == 1 would false-match.
        if isinstance(every_ms, bool) or not isinstance(every_ms, (int, float)):
            return False
        wanted = _parse_duration_ms(spec.schedule_value)
        return wanted is not None and int(every_ms) == wanted
    if stored.get("kind") != "cron":
        return False
    expr = stored.get("expr")
    if not isinstance(expr, str):
        return False
    return _collapse_ws(expr) == _collapse_ws(spec.schedule_value)


def _schedule_is_current(job: dict, spec: JobSpec) -> bool:
    """Union schedule comparison.

    A stored dict carrying a 'kind' key is the CLI contract shape and is
    compared precisely (never through the fallback). Any other
    recognizable shape — a plain-string schedule, or a dict carrying
    expression/cron/every/value string keys — compares whitespace-collapsed
    against the literal spec value and nothing else (no every→cron
    conversion: a stored cron-form for an every-spec is drift). Anything
    unreadable is drift: fail open to a `cron edit` re-assert, never a
    silent skip."""
    stored = job.get("schedule")
    if isinstance(stored, dict) and "kind" in stored:
        return _kind_schedule_is_current(stored, spec)
    literal = _stored_schedule_string(job)
    if literal is None:
        return False
    return literal == _collapse_ws(spec.schedule_value)


def _job_is_current(job: dict, spec: JobSpec) -> bool:
    """Current requires message, threadId (when the spec has a topic),
    delivery.to (when CHAT is set), and schedule to all match."""
    msg = ""
    payload = job.get("payload") or {}
    if isinstance(payload, dict) and payload.get("kind") == "agentTurn":
        msg = payload.get("message", "") or ""
    delivery = job.get("delivery") or {}
    stored_thread = delivery.get("threadId") if isinstance(delivery, dict) else None
    stored_to = delivery.get("to") if isinstance(delivery, dict) else None
    msg_match = msg == spec.prompt
    thread_match = spec.topic == "" or str(stored_thread) == str(spec.topic)
    chat_match = CHAT == "" or str(stored_to) == str(CHAT)
    return msg_match and thread_match and chat_match and _schedule_is_current(job, spec)


def reconcile(spec: JobSpec, jobs: list[dict], model: str) -> None:
    """Converge the live job for `spec`: prune same-name duplicates
    (keeping the delivery routed to CHAT), create via `cron add`, or edit
    when message/thread/chat/schedule drifted. Skips when current."""
    matches = [j for j in jobs if isinstance(j, dict) and j.get("name") == spec.name]

    if len(matches) > 1:

        def sort_key(j: dict) -> tuple[bool, str]:
            delivery = j.get("delivery") or {}
            to = str(delivery.get("to", "")) if isinstance(delivery, dict) else ""
            return (to == CHAT, j.get("id", ""))

        matches.sort(key=sort_key)
        keeper = matches[-1]
        for j in matches[:-1]:
            job_id = j.get("id", "")
            if not job_id:
                continue
            result = openclaw(["cron", "delete", job_id])
            if result.returncode == 0:
                print(f"[seed-automations] pruned duplicate '{spec.name}' job: {job_id}")
            else:
                print(
                    f"[seed-automations] WARNING: failed to delete "
                    f"duplicate '{spec.name}' job: {job_id}"
                )
        matches = [keeper]

    if not matches:
        flags = delivery_flags(spec)
        print(
            f"[seed-automations] creating '{spec.name}' "
            f"({spec.schedule_flag} {spec.schedule_value})"
        )
        result = openclaw(
            [
                "cron",
                "add",
                spec.schedule_flag,
                spec.schedule_value,
                "--message",
                spec.prompt,
                "--name",
                spec.name,
                "--agent",
                "main",
                "--session",
                "isolated",
                "--model",
                model,
                *flags,
            ]
        )
        if result.returncode != 0:
            print(
                f"[seed-automations] WARNING: cron add failed: {spec.name}: {result.stderr.strip()}"
            )
        return

    job = matches[0]
    if _job_is_current(job, spec):
        print(f"[seed-automations] '{spec.name}' already current — skipping")
        return

    job_id = job.get("id", "")
    print(f"[seed-automations] updating '{spec.name}'")
    flags = delivery_flags(spec)
    result = openclaw(
        [
            "cron",
            "edit",
            job_id,
            "--message",
            spec.prompt,
            spec.schedule_flag,
            spec.schedule_value,
            *flags,
        ]
    )
    if result.returncode != 0:
        print(f"[seed-automations] WARNING: cron edit failed: {spec.name}: {result.stderr.strip()}")


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(
        description="Reconcile OpenClaw cron jobs from markdown specs."
    )
    parser.add_argument(
        "--model",
        default="",
        help="model for cron agent turns (overrides AUTOMATION_MODEL)",
    )
    args = parser.parse_args(argv)

    try:
        model = resolve_model(args.model)
        specs = build_jobs()
    except (AutomationSpecError, ModelResolutionError) as exc:
        print(f"[seed-automations] ERROR: {exc}", file=sys.stderr)
        print(
            "[seed-automations] aborting — no jobs created, edited, or pruned",
            file=sys.stderr,
        )
        sys.exit(1)

    # One listing for the whole run: reconcile operations only touch their
    # own job (names are unique per spec), so per-spec listings would spend
    # N CLI spawns to observe the same state.
    jobs = list_cron_jobs()
    if jobs is None:
        print(
            "[seed-automations] ERROR: cannot parse cron list — aborting to prevent duplicates",
            file=sys.stderr,
        )
        sys.exit(1)

    for spec in specs:
        try:
            reconcile(spec, jobs, model)
        except Exception as exc:
            print(
                f"[seed-automations] ERROR reconciling '{spec.name}': {exc}",
                file=sys.stderr,
            )
    print("[seed-automations] done.")


if __name__ == "__main__":
    main()
