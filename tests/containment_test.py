"""Containment harness contract tests (the Python half of internal/e2e)."""

import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, os.path.dirname(__file__))

import containment  # noqa: E402 — module-attribute access per repo discipline


class ContainedStepTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())

    def test_clean_step_leaves_artifacts(self):
        artifact_dir = containment.run_step("ok", ["true"], budget=10, artifact_root=self.root)
        self.assertTrue(artifact_dir.exists())
        self.assertTrue((artifact_dir / "stderr.log").exists())

    def test_budget_breach_kills_group_and_dumps(self):
        seen = {}
        dumped = []

        def dumper(artifact_dir: Path):
            dumped.append(artifact_dir)
            (artifact_dir / "extra.txt").write_text("evidence", encoding="utf-8")

        with self.assertRaises(containment.ContainedFailure) as caught:
            containment.run_step(
                "wedge",
                ["sh", "-c", "sleep 5 & sleep 5 & wait"],
                budget=1.0,
                artifact_root=self.root,
                dumpers=(dumper,),
                on_start=lambda pgid: seen.update(pgid=pgid),
            )
        self.assertTrue(caught.exception.timed_out)
        self.assertIn("artifacts:", str(caught.exception))
        self.assertEqual(len(dumped), 1)
        self.assertTrue((dumped[0] / "extra.txt").exists())
        # The WHOLE group died: no survivor answers the group signal.
        with self.assertRaises(ProcessLookupError):
            os.killpg(seen["pgid"], 0)

    def test_nonzero_exit_carries_tail(self):
        with self.assertRaises(containment.ContainedFailure) as caught:
            containment.run_step(
                "boom",
                ["sh", "-c", "echo pre-failure evidence; exit 3"],
                budget=10,
                artifact_root=self.root,
            )
        self.assertFalse(caught.exception.timed_out)
        self.assertIn("pre-failure evidence", str(caught.exception))
        self.assertIn("exited 3", str(caught.exception))

    def test_dumper_error_does_not_mask_failure(self):
        def exploding(artifact_dir: Path):
            raise RuntimeError("dumper exploded")

        with self.assertRaises(containment.ContainedFailure):
            containment.run_step(
                "panic-dumper",
                ["false"],
                budget=10,
                artifact_root=self.root,
                dumpers=(exploding,),
            )
        matches = list(self.root.glob("panic-dumper-*"))
        self.assertEqual(len(matches), 1)
        self.assertTrue((matches[0] / "dumper-0-error.txt").exists())

    def test_compose_dumpers_sink_output(self):
        captured = {}

        def fake_sink(engine, project, verb, dest):
            captured["argv"] = [engine, project, *verb]
            captured["dest"] = dest
            dest.write_text("sunk", encoding="utf-8")

        original = containment._sink
        containment._sink = fake_sink
        try:
            dumpers = containment.compose_dumpers("podman", "e2e-plane")
            self.assertEqual(len(dumpers), 2)
            artifact_dir = self.root / "plane-artifacts"
            artifact_dir.mkdir()
            dumpers[0](artifact_dir)
        finally:
            containment._sink = original
        self.assertEqual(captured["argv"], ["podman", "e2e-plane", "ps", "-a"])
        self.assertEqual(captured["dest"].name, "compose-ps.txt")
        self.assertEqual(captured["dest"].read_text(encoding="utf-8"), "sunk")


if __name__ == "__main__":
    unittest.main()
