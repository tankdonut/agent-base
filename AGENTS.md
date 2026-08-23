# Agent Instructions

## Commands

| Task | Command |
| ----- | ------- |
| All tests | `./make.sh test` |
| One module | `python3 -m unittest discover -s container -p "test_spec.py"` |
| Lint (ruff, ruff-format, hadolint, markdownlint) | `./make.sh lint` |
| Image smoke (local only; podman or docker) | `./make.sh smoke` |
| Build image (date tag default) | `./make.sh build` |
| Push image | `AGENT_BASE_VERSION=YYYY.MM.DD ./make.sh push` — refuses implicit tags |
| Validate a spec (CI gate) | `docker run --rm --env-file .env <image> --validate-spec` |
| CLI contract (CI + local) | `scripts/contract-test.sh <image>` — real-CLI drift gate |

## Structure

```text
container/   Image contract: entrypoint.py (boot), spec.py (loader), seed_automations.py
             (cron reconciler), Dockerfile, colocated test_*.py (never shipped)
docs/        standard-agent.md — the whole agent contract + Freya/Mimir migration guides
fixtures/    freya-like/, mimir-like/ — boot-tested spec+automations trees; input for
             unit FixtureBoots and smoke; consult, don't copy whole
scripts/     smoke.sh (shim harness), contract-test.sh + contract/ (real-CLI drift
             gate: emitted-flag cross-check vs --help + clean shim-free boot),
             shim/openclaw (fake CLI; asserts via invocation log)
templates/   spec.example.json (golden), env.example, compose snippets, workspace skeletons
```

## Where To Look

| Task | Location |
| ----- | -------- |
| Change boot behavior | `container/entrypoint.py` — each phase is a plain function over `(spec, env)` |
| Spec schema change | `container/spec.py` + `templates/spec.example.json` + `docs/standard-agent.md` (same commit) |
| Cron reconcile behavior | `container/seed_automations.py` |
| Env var contract (base vs project) | `templates/env.example`, `docs/standard-agent.md#environment-contract` |
| Smoke failure | `.smoke-*.log` (kept on failure, deleted on success) + `scripts/smoke.sh` |
| Migration guides | `docs/standard-agent.md#migrations` |

## Code Map

Symbols relative to `container/`.

| Symbol | Type | Location | Role |
| ------ | ---- | -------- | ---- |
| `main` | fn | entrypoint.py:631 | Phase orchestration; happy path ends in `os.execvp` — int returns only for `--validate-spec` (0/1) and usage (2) |
| `load_agent_spec` | fn | entrypoint.py:114 | Fail-closed load; `AGENT_SPEC_PATH` override |
| `first_boot_setup` | fn | entrypoint.py:211 | One-time setup; gated on `openclaw.json` absent |
| `reconcile_config` / `reconcile_mcp` / `reconcile_plugins` | fn | entrypoint.py:259 / :290 / :317 | Idempotent reconcile; warn-never-raise |
| `authenticate_gh` | fn | entrypoint.py:334 | gh auth from `AGENT_GIT_TOKEN`; every boot, non-fatal |
| `seed_content` | fn | entrypoint.py:372 | workspace first boot only; skills + docs full replace every boot |
| `post_startup` | fn | entrypoint.py:572 | Forked child: gateway wait ≤180s, cron seed in-process, memory reindex, doctor |
| `load_spec` | fn | spec.py:527 | Strict v1 loader; errors carry the JSON path |
| `Spec` / `SpecError` | cls | spec.py | Frozen spec / `ValueError` subtype |
| `LocalMcpServer` / `RemoteMcpServer` | cls | spec.py:114 / :132 | stdio vs HTTP MCP; exactly one of `command` / `url` |
| `build_jobs` | fn | seed_automations.py | Parse `automations/*.md` fail-closed |
| `reconcile` | fn | seed_automations.py | Idempotent cron add/edit; heals drift with one edit |

## Key Conventions

- Python 3.11 floor (bookworm image), stdlib only; no 3.12+ syntax; full type annotations (convention — no mypy gate); `unittest` + `mock`, never pytest. Stdlib-only is enforced by construction: the image installs no pip; `.ruff.toml` targets py311, line 100; CI matrix is 3.11 (floor) + 3.14 (drift guard).
- Loader modules fail closed: unknown key/token, ambiguous shape → abort loudly; never a silent empty string or skip.
- Secrets flow only through `{env:VAR}` spec refs and env vars; resolved values must never reach logs — warnings name keys/env vars, never values (locked by SecretsCanary tests).
- `container/` files are the image contract; renaming/moving any of them changes downstream projects' Dockerfiles — update `docs/standard-agent.md` in the same commit. Modules import each other top-level (no package, no `__init__.py`); the Dockerfile COPYs exactly the three modules flat to `/opt/agent`.
- Reconcile failures warn and never raise (gateway availability > config completeness); loader failures abort the boot.
- Formatter is `ruff-format` (not black) via pre-commit, alongside ruff, hadolint (`container/Dockerfile` only), markdownlint (`fixtures/**` ignored).
- CI composes reusable actions from `tankdonut/github-actions` (`pre-commit`, `setup-python-uv`, `build-and-publish-image.yaml`); do not hand-roll equivalents.
- Images publish under date tags only (`YYYY.MM.DD`); no `latest` exists; push requires explicit `AGENT_BASE_VERSION`.

## Anti-Patterns

- Baked default for `automations.model` — hard error naming `--model` + `AUTOMATION_MODEL`; defaults drift silently between agents sharing the image.
- Bind-mounting automations in any mode (incl. dev overlay) — writable cron prompts are an agent self-modification surface.
- Setting `OPENCLAW_HOME` — double-nests `{data}`; the entrypoint pops it at import.
- Writing docs to `{data}/docs` — the hard standard is `{data}/workspace/docs`.
- Floating image tags; hand-rolled CI steps.
- Editing `AGENTS.md` under `templates/workspace/` or `fixtures/*/workspace/` as project docs — those are shipped agent personas (payload), not repo documentation.
- In tests: `from seed_automations import X` — reload discipline requires module-attribute access (`seed_automations.X`).

## Unique Styles

- `# allow: SIZE_OK` header marks contractually-single test files exempt from size ceilings.
- Meta-tests: an AST audit of `entrypoint.py` forbids legacy `FREYA_` / `MIMIR_` env names; import-safety classes assert importing never boots.
- Secrets canary tests: plant a canary, assert it reaches CLI argv but never captured stdout/stderr.
- Smoke asserts phase order by line number in the shim invocation log (`scripts/shim/openclaw`), plus proof-of-absence checks (e.g. no `memory index` on clean status).

## Notes

- Smoke is local-only (not in CI); logs are `.smoke-*.log` — kept on failure, removed on success.
- `scripts/smoke.sh` embeds a Python RUNNER mirroring `main()` minus fork/execvp — update both when phases change.
- Project one-offs go in wrapper entrypoints that import the phases — never hooks in the base (docs "Escape hatch: wrapper entrypoints").
- Local `container/__pycache__` (cpython-313/314) and the `.codegraph` symlink are machine-local, untracked artifacts.

## External References

| Need | File |
| ----- | ----- |
| Agent contract + project extension guide | `docs/standard-agent.md` |
| Migration guides (Freya, Mimir) | `docs/standard-agent.md#migrations` |
| Spec schema golden example | `templates/spec.example.json` |
| Env contract (base vs project vars) | `templates/env.example` |
