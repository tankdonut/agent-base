---
name: releasing-agent-base
description: Use when asked to release agent-base, cut a release, publish a new version/image tag, ship the current work ("ship it", "release this"), make a same-day follow-up release, or investigate why a tag has no GitHub release.
---

# Releasing agent-base

## Overview

A release is **one signed annotated date tag pushed to origin** — but the
bytes it publishes were qualified **before** the tag exists. The flow is
two-phase: a main push whose `VERSION` file names a new date tag bakes
per-arch candidate images in the private staging package, runs every gate
against the exact index digest, and issues a receipt; the tag push then
promotes those bytes verbatim (no rebuild). Your job: bump `VERSION`,
pick the version, tag correctly, watch the runs, verify the artifacts,
and fix forward if a job fails.

## Version scheme (hard rules)

- Image: `ghcr.io/tankdonut/agent-base`, date tags only: `YYYY.MM.DD`.
  Per-arch companions `<tag>-amd64`/`<tag>-arm64` are tagged at promotion.
- No `latest`. Published tags are immutable — never move, re-point, or
  delete one.
- Same-day follow-up: append a run suffix — `2026.08.24`, then
  `2026.08.24.1`, `.2`, … Each suffix is a full independent release with
  its own image and GitHub release.
- `VERSION` (repo root) names the release intent: it must equal the tag
  verbatim, including any `.N` suffix.

## Procedure

### 1. Pre-flight

```sh
git fetch --tags origin
git status --porcelain              # must be empty
git rev-parse HEAD main origin/main # must all agree; release from main tip
gh run list --branch main --limit 3 # last main push must be green
scripts/check-image-refs.sh         # every tracked <image>:<date> ref exists in GHCR
```

The refs script scans ALL tracked files — docs and test fixtures alike.
Hypothetical tags in fixtures (`2026.08.28.2`, a future `2026.09.01`) are
the 2026.08.29 failure: the release-job gate caught them only after image
publish had burned the tag. Run it here and an unpublished ref costs a
one-line fixture edit, not a `.N` re-release.

Pick the version: `$(date +%Y.%m.%d)`. If that tag already exists (check
local **and** remote — the remote is what raced you), take the highest
existing suffix and increment:

```sh
git tag -l '2026.08.28*' | sort -V | tail -1    # highest existing (local)
git ls-remote --tags origin 'refs/tags/2026.08.28*'
```

### 2. Bump VERSION (on main, before tagging)

The root `VERSION` file ships in every release and must equal the tag
verbatim (including any `.N` suffix):

```sh
echo 2026.08.28 > VERSION
git commit -am "chore(release): bump version to 2026.08.28"
git push origin main
gh run list --branch main --limit 1   # watch to green before tagging
```

That main push IS the candidate qualification: per-arch builds land in
`ghcr.io/tankdonut/agent-base-staging`, the multi-arch index is verified
(HEALTHCHECK, baked version), gates run against the exact digest
(smoke + contract on both arches, arm64 boot sanity), attestations are
pushed, and a receipt artifact binds sha ↔ version ↔ digests ↔ suite
results. A VERSION naming an already-released tag skips the flow (plain
CI), so a forgotten bump surfaces at tag time as "no successful
candidate run" — fix-forward with the bump + next tag, never a re-tag.

### 3. Tag and push (this triggers promotion)

```sh
git tag -a 2026.08.28 -m "agent-base 2026.08.28 — <one-line summary of what this release carries>"
git push origin 2026.08.28
```

- Annotated; SSH signing is automatic from repo config (`tag.gpgsign=true`).
- Message convention, every past release: `agent-base <tag> — <summary>`.
- Tag main's tip (the bump commit). Never tag a dirty tree, a non-main
  commit, or an unpushed commit.

### 4. Watch the pipeline

The tag run is validation + copy, never a rebuild: it resolves the
successful candidate run for the tagged commit, downloads the receipt,
greenlights it (sha, version, all suites), promotes the qualified index
with `oras cp --recursive`, asserts digest identity, tags the per-arch
children `<tag>-amd64`/`<tag>-arm64` (cosmetic, never blocks the
release), then publishes the GitHub release (SBOM, receipt,
digest-pinned notes).

```sh
gh run list --branch 2026.08.28 --limit 1   # tag runs carry the tag as branch
gh run watch <run-id> --exit-status
```

### 5. Verify

```sh
gh release view 2026.08.28            # exists; assets: sbom.spdx.json, receipt.json; digest + pin line
docker buildx imagetools inspect ghcr.io/tankdonut/agent-base:2026.08.28   # amd64+arm64
gh attestation verify oci://ghcr.io/tankdonut/agent-base:2026.08.28@sha256:<digest-from-notes> -R tankdonut/agent-base
```

Every check green = release complete. (Renovate then opens digest-bump PRs in
consumer repos on its own — not your concern here.)

## Failure playbook

A tag with no GitHub release is a known historical state (`2026.08.24`,
`.2`, `.3`, and `2026.08.29` all published images but never released). Handle failures by
class:

1. **Read the failure**: `gh run view <run-id> --log-failed`.
2. **Classify**: transient = infrastructure only (ghcr/GitHub 5xx, DNS,
   runner eviction) with no repo code or config implicated → re-run.
   Anything deterministic, or pointing at a file/command in this repo →
   real bug.
3. **Transient** (network flake, runner issue): `gh run rerun <run-id> --failed`.
4. **Real bug**: fix on `main`, ship the fix, cut the **next same-day
   suffix** tag from the new main tip. Never delete/re-push the broken
   tag; never hand-run `gh release create` to paper over a failed job.

Known failure classes (all with shipped fixes — cite for pattern, not
novelty): attest without ghcr login; HEALTHCHECK gate jq path / blob
redirects; refs gate failing because a tracked doc references a
never-published tag (fix the doc ref on main, then re-release `.N`);
refs gate tripped by hypothetical tags in **test fixtures** — the
2026.08.29 case, caught after image publish burned the tag, fixed by
pointing fixtures at published tags and re-releasing as `.1` (the
pre-flight refs script now catches this class before tagging).

## Common mistakes

| Mistake | Reality |
| --- | --- |
| `AGENT_BASE_VERSION=<tag> ./make.sh push` to publish the release | CI is the only publisher — multi-arch needs the native arm64 runner; a local push is single-arch and collides with the tag run |
| Bumping `ARG AGENT_BASE_VERSION` in `container/Dockerfile` at release time | CI bakes `--build-arg AGENT_BASE_VERSION=<tag>`; the Dockerfile default is for bare local builds only |
| Re-tagging the same version after a failure | Tags are immutable; the fix-forward is the next `.N` suffix |
| Hand-writing release notes | The release job generates the digest, pin line, changelog, and provenance command; manual edits break the consumer contract |
| Tagging a commit not on main | Releases are cut from main; pre-flight catches this |
| Tagging without the VERSION bump | The release job finds no candidate run for the commit (VERSION never named the tag) and refuses; fix-forward with the bump + next `.N` |

## Quick reference

| Step | Command |
| --- | --- |
| Existing tags | `git tag -l '20*'`; `git ls-remote --tags origin` |
| Bump version | `echo <tag> > VERSION` → `chore(release): bump version to <tag>` → push main, watch to green |
| Cut | `git tag -a <tag> -m "agent-base <tag> — <summary>"` → `git push origin <tag>` |
| Watch | `gh run list --branch <tag> --limit 1` → `gh run watch <run-id> --exit-status` |
| Failed-job log | `gh run view <run-id> --log-failed` |
| Rerun transient | `gh run rerun <run-id> --failed` |
| Verify | `gh release view <tag>`; `docker buildx imagetools inspect ghcr.io/tankdonut/agent-base:<tag>`; `gh attestation verify oci://ghcr.io/tankdonut/agent-base:<tag>@sha256:<digest> -R tankdonut/agent-base` |
