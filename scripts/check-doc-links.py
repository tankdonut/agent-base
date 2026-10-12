#!/usr/bin/env python3
"""Doc link/anchor gate for the repo documentation set.

Validates every markdown link in the doc set (README.md, AGENTS.md,
container/AGENTS.md, docs/*.md): relative targets must exist on disk, and
intra-doc #anchors must match a heading in the target file. Anchor slugs
follow GitHub rules: lowercase, punctuation stripped (hyphens kept), spaces
to hyphens, duplicate headings suffixed -1, -2, ...

External URLs (http/https/mailto/ftp/...) are skipped: the gate is offline
by design. Fenced code blocks are ignored, whatever their indent. The full
set is checked on every run (not just changed files) because anchors live
across files: renaming a heading in one doc breaks links in another.

Excluded by scope: tests/fixtures/** and examples/workspace/** (machine-
parsed specs and shipped personas, not repo documentation).
"""

import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
DOC_FILES: list[Path] = [
    REPO_ROOT / "README.md",
    REPO_ROOT / "AGENTS.md",
    REPO_ROOT / "container" / "AGENTS.md",
    *sorted((REPO_ROOT / "docs").glob("*.md")),
]

FENCE_RE = re.compile(r"^\s*(```|~~~)")
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*#*\s*$")
LINK_RE = re.compile(r"\[[^\]\[]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
EXTERNAL_RE = re.compile(r"^(https?|ftp|mailto|git|ssh):", re.IGNORECASE)

_anchors_cache: dict[Path, set[str]] = {}


def strip_fences(lines: list[str]) -> list[str]:
    """Blank out fenced code blocks, preserving line count and numbering."""
    out: list[str] = []
    in_fence = False
    for line in lines:
        if FENCE_RE.match(line):
            in_fence = not in_fence
            out.append("")
        else:
            out.append("" if in_fence else line)
    return out


def slugify(heading: str) -> str:
    """GitHub anchor slug: lowercase, strip punctuation, spaces to hyphens."""
    s = heading.strip().lower()
    s = re.sub(r"[^\w\s-]", "", s)
    return re.sub(r"\s+", "-", s)


def heading_anchors(lines: list[str]) -> set[str]:
    """All valid anchors for a file, with -N suffixes for duplicate slugs."""
    counts: dict[str, int] = {}
    anchors: set[str] = set()
    for line in lines:
        m = HEADING_RE.match(line)
        if not m:
            continue
        base = slugify(m.group(2))
        if not base:
            continue
        n = counts.get(base, 0)
        counts[base] = n + 1
        anchors.add(base if n == 0 else f"{base}-{n}")
    return anchors


def anchors_of(path: Path) -> set[str]:
    if path not in _anchors_cache:
        _anchors_cache[path] = heading_anchors(strip_fences(path.read_text().splitlines()))
    return _anchors_cache[path]


def check_file(path: Path) -> list[str]:
    lines = strip_fences(path.read_text(encoding="utf-8").splitlines())
    own = anchors_of(path)
    problems: list[str] = []
    for lineno, line in enumerate(lines, start=1):
        for m in LINK_RE.finditer(line):
            target = m.group(1)
            if EXTERNAL_RE.match(target) or target.startswith("//"):
                continue
            file_part, _, fragment = target.partition("#")
            if not file_part:
                if fragment and fragment not in own:
                    problems.append(f"{path}:{lineno}: anchor '#{fragment}' not found in this file")
                continue
            resolved = (path.parent / file_part).resolve()
            if not resolved.exists():
                problems.append(f"{path}:{lineno}: target '{file_part}' does not exist")
                continue
            if fragment and resolved.suffix == ".md" and fragment not in anchors_of(resolved):
                where = resolved.relative_to(REPO_ROOT)
                problems.append(f"{path}:{lineno}: anchor '#{fragment}' not found in {where}")
    return problems


def main() -> int:
    problems: list[str] = []
    for doc in DOC_FILES:
        if doc.exists():
            problems.extend(check_file(doc))
    if problems:
        print("[doc-links] broken links/anchors:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        return 1
    print(f"[doc-links] {len(DOC_FILES)} doc file(s) checked — no broken links or anchors")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
