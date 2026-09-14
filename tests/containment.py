"""Containment harness for engine-touching e2e steps (Python half).

Contract mirrors internal/e2e (Go): every engine child runs in its own
process group (start_new_session) under a hard budget; a breach costs
the whole group (SIGKILL to -pgid, so grandchildren die too) plus an
artifact dump — the child's stderr and any step-specific dumpers —
under logs/e2e/<name>-<stamp>/. A wedge fails loudly with evidence
instead of hanging the suite.
"""

from __future__ import annotations

import contextlib
import os
import signal
import subprocess
import time
from collections.abc import Callable
from pathlib import Path

DEFAULT_ARTIFACT_ROOT = Path("logs/e2e")
TAIL_LINES = 40


class ContainedFailure(Exception):
    """A contained step failed (non-zero exit, breach, or cancel)."""

    def __init__(self, message: str, artifact_dir: Path, timed_out: bool = False):
        super().__init__(message)
        self.artifact_dir = artifact_dir
        self.timed_out = timed_out


def _tail(path: Path) -> str:
    if not path.exists():
        return ""
    lines = path.read_text(errors="replace").splitlines()
    return "\n".join(lines[-TAIL_LINES:])


def _dump(dumpers, artifact_dir: Path) -> None:
    """Run step-specific dumpers, absorbing their errors — a failing
    dumper must not mask the original failure."""
    for i, dumper in enumerate(dumpers):
        try:
            dumper(artifact_dir)
        except Exception as exc:  # noqa: BLE001 — evidence over purity
            (artifact_dir / f"dumper-{i}-error.txt").write_text(repr(exc), encoding="utf-8")


def run_step(
    name: str,
    argv: list[str],
    *,
    cwd: Path | None = None,
    budget: float = 240.0,
    artifact_root: Path | None = None,
    dumpers: tuple[Callable[[Path], None], ...] = (),
    env: dict[str, str] | None = None,
    on_start: Callable[[int], None] | None = None,
) -> Path:
    """Run one contained engine command; return the artifact dir.

    Raises ContainedFailure on budget breach or non-zero exit.
    """
    root = artifact_root or DEFAULT_ARTIFACT_ROOT
    stamp = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
    artifact_dir = root / f"{name}-{stamp}"
    artifact_dir.mkdir(parents=True, exist_ok=True)
    stderr_path = artifact_dir / "stderr.log"

    with open(stderr_path, "wb") as stderr:
        proc = subprocess.Popen(
            argv,
            cwd=cwd,
            env=env,
            stdout=stderr,
            stderr=subprocess.STDOUT,
            start_new_session=True,  # the child IS the process group
        )
    if on_start is not None:
        on_start(os.getpgid(proc.pid))
    try:
        proc.wait(timeout=budget)
    except subprocess.TimeoutExpired:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        proc.wait()
        _dump(dumpers, artifact_dir)
        raise ContainedFailure(
            f"{name}: breached its {budget}s budget (artifacts: {artifact_dir})\n"
            f"{_tail(stderr_path)}",
            artifact_dir,
            timed_out=True,
        ) from None
    if proc.returncode != 0:
        _dump(dumpers, artifact_dir)
        raise ContainedFailure(
            f"{name}: exited {proc.returncode} (artifacts: {artifact_dir})\n{_tail(stderr_path)}",
            artifact_dir,
        )
    return artifact_dir


def compose_dumpers(engine: str, project: str) -> tuple[Callable[[Path], None], ...]:
    """Standard dumpers for compose steps: ps state + per-container logs."""

    def ps(artifact_dir: Path) -> None:
        _sink(engine, project, ["ps", "-a"], artifact_dir / "compose-ps.txt")

    def logs(artifact_dir: Path) -> None:
        _sink(engine, project, ["logs", "--tail", "100"], artifact_dir / "compose-logs.txt")

    return (ps, logs)


def _sink(engine: str, project: str, verb: list[str], dest: Path) -> None:
    argv = [engine, "compose", "-p", project, *verb]
    try:
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=60, check=False)
        dest.write_text(proc.stdout + proc.stderr, encoding="utf-8")
    except Exception as exc:  # noqa: BLE001 — evidence over purity
        dest.write_text(repr(exc), encoding="utf-8")
