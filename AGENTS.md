# Agent Instructions

## Commands

| Task | Command |
| ----- | ------- |
| All tests | `./make.sh test` |
| One module | `python3 -m unittest discover -s container -p "test_spec.py"` |
| agentctl (Go) tests | `./make.sh agentctl-test` |
| agentctl build/install | `./make.sh agentctl-build` / `./make.sh agentctl-install` |
| Run agentctl without installing | `./make.sh agentctl <command> [flags]` (forwards to `go run`) |
| Lint (ruff, ruff-format, hadolint, markdownlint) | `./make.sh lint` |
| Image smoke (CI + local; podman or docker, `SMOKE_ENGINE` override) | `./make.sh smoke` |
| Build image (date tag default) | `./make.sh build` |
| Push image | `AGENT_BASE_VERSION=YYYY.MM.DD[.N] ./make.sh push` — refuses implicit tags; same-day follow-up releases use the `.N` run suffix |
| Validate a spec (CI gate) | `docker run --rm --env-file .env <image> --validate-spec` |
| CLI contract (CI + local) | `scripts/contract-test.sh <image>` — real-CLI drift gate |

## Structure

```text
cmd/         agentctl CLI — operator tool for downstream agent repos (scaffold first)
container/   Image contract: entrypoint.py (boot), spec.py (loader), seed_automations.py
             (cron reconciler), Dockerfile, colocated test_*.py (never shipped)
docs/        standard-agent.md — the whole agent contract + Freya/Mimir migration guides
fixtures/    freya-like/, mimir-like/ — boot-tested spec+automations trees; input for
             unit FixtureBoots and smoke; consult, don't copy whole
internal/    agentctl engine: cli (cobra), lifecycle (cobra-free), scaffold,
             embedded templates (templates/tmpl)
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
| Graceful shutdown / drain behavior | `container/entrypoint.py` — `supervise`, `ShutdownSupervisor`, `parse_shutdown_grace` |
| Env var contract (base vs project) | `templates/env.example`, `docs/standard-agent.md#environment-contract` |
| Smoke failure | `logs/smoke-*.log` (kept on failure, deleted on success) + `scripts/smoke.sh` |
| Migration guides | `docs/standard-agent.md#migrations` |
| Scaffold a new downstream agent repo | `cmd/agentctl` — `go run ./cmd/agentctl init <dir>` |

## Code Map

Symbols relative to `container/`.

| Symbol | Type | Location | Role |
| ------ | ---- | -------- | ---- |
| `main` | fn | entrypoint.py:1375 | Phase orchestration; forks post_startup then `supervise()`s the CMD — returns its exit code after graceful-shutdown drain; other int returns: `--validate-spec` (0/1) and usage (2) |
| `supervise` / `ShutdownSupervisor` / `parse_shutdown_grace` | fn/cls | entrypoint.py:1350 / :1206 / :1187 | Graceful shutdown: CMD runs in its own process group; first SIGTERM/SIGINT forwards to the CMD pid only, the drain waits for the group to empty up to `AGENT_SHUTDOWN_GRACE` (default 600; 0 = forward + immediate force-kill), a second signal force-kills, an unprompted CMD exit kills the group (restart semantics); exit code = CMD's, 128+N when signaled |
| `backup_before_upgrade` | fn | entrypoint.py:888 | Verified backup on `AGENT_BASE_VERSION` delta (warm volume); failure aborts — data safety beats availability for migrations |
| `load_agent_spec` | fn | entrypoint.py:125 | Fail-closed load; `AGENT_SPEC_PATH` override |
| `first_boot_setup` | fn | entrypoint.py:222 | One-time setup; gated on `openclaw.json` absent; snapshots base plugin installs to `{data}/agent-managed-plugins` |
| `reconcile_config` / `reconcile_mcp` / `reconcile_plugins` | fn | entrypoint.py:302 / :333 / :402 | Idempotent reconcile; warn-never-raise; MCP removal gated on `features.mcp_prune`, plugin prune on `features.plugin_prune` (ownership markers under `{data}`); MCP + plugin orphan reports are warn-only |
| `authenticate_gh` | fn | entrypoint.py:464 | gh auth from `AGENT_GIT_TOKEN`; every boot, non-fatal |
| `seed_content` | fn | entrypoint.py:502 | workspace first boot only; skills + docs full replace every boot |
| `post_startup` | fn | entrypoint.py:780 | Forked child: gateway wait ≤180s, cron seed in-process, memory reindex, doctor skills reconcile, diagnostics (doctor/security reports to `{data}/logs`, boot summary `{data}/status.json`) |
| `load_spec` | fn | spec.py:527 | Strict v1 loader; errors carry the JSON path |
| `Spec` / `SpecError` | cls | spec.py | Frozen spec / `ValueError` subtype |
| `LocalMcpServer` / `RemoteMcpServer` | cls | spec.py:114 / :132 | stdio vs HTTP MCP; exactly one of `command` / `url` |
| `build_jobs` | fn | seed_automations.py | Parse `automations/*.md` fail-closed |
| `reconcile` | fn | seed_automations.py | Idempotent cron add/edit; heals drift with one edit |

## Key Conventions

- Python 3.11 floor (bookworm image), stdlib only; no 3.12+ syntax; full type annotations (convention — no mypy gate); `unittest` + `mock`, never pytest. Stdlib-only is enforced by construction: the image installs no pip; `.ruff.toml` targets py311, line 100; CI matrix is 3.11 (floor) + 3.14 (drift guard).
- Go module at the repo root (`agentctl`, spf13/cobra + viper): operator CLI for downstream projects (scaffold + lifecycle + secrets + validate); scaffold templates embedded under `internal/templates/tmpl`; lifecycle logic stays cobra-free in `internal/lifecycle` behind a Runner interface.
- Loader modules fail closed: unknown key/token, ambiguous shape → abort loudly; never a silent empty string or skip.
- Secrets flow only through `{env:VAR}` spec refs and env vars; resolved values must never reach logs — warnings name keys/env vars, never values (locked by SecretsCanary tests).
- `container/` files are the image contract; renaming/moving any of them changes downstream projects' Dockerfiles — update `docs/standard-agent.md` in the same commit. Modules import each other top-level (no package, no `__init__.py`); the Dockerfile COPYs exactly the three modules flat to `/opt/agent`.
- Reconcile failures warn and never raise (gateway availability > config completeness); loader failures abort the boot.
- Seeded automations run with a bounded tool allow-list (`seed_automations.DEFAULT_JOB_TOOLS` — fs/runtime/web/memory + `bundle-mcp`; recursion/spawn/browser excluded, OWASP ASI06). Per-job `tools:` header or spec `automations.default_tools` overrides; `*` = unrestricted. The base also sets `tools.deny` (cron, subagents, sessions_spawn, nodes) unless any env-active spec `tools.*` config entry exists (an `if_env` guard that never fires configures nothing).
- Formatter is `ruff-format` (not black) via pre-commit, alongside ruff, hadolint (`container/Dockerfile` only), markdownlint (`fixtures/**` ignored).
- CI composes reusable actions from `tankdonut/github-actions` (`pre-commit`, `setup-python-uv`, `ghcr-login`); do not hand-roll equivalents. Exception: the multi-arch image jobs in `.github/workflows/ci.yml` use native per-arch runners + `imagetools` merge — the shared `build-and-publish` workflow hardcodes `ubuntu-latest` and cannot express per-arch builds (and a single multi-platform buildx push drops the HEALTHCHECK via the OCI exporter).
- Releases bump two Go constants on main before tagging: `internal/cli` `Version` and `internal/scaffold` `DefaultBaseTag`, both equal to the tag verbatim. The `release` job cross-compiles `agentctl-linux-amd64/arm64` + `agentctl-SHA256SUMS` into the release assets and fails if `agentctl version` ≠ tag (see the `releasing-agent-base` skill).
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

- Smoke runs in CI (`smoke` job, docker via `SMOKE_ENGINE`) and locally (podman-first); logs are `logs/smoke-*.log` (gitignored) — kept on failure, removed on success.
- `scripts/smoke.sh` embeds a Python RUNNER mirroring `main()` minus the fork/supervise handoff — update both when phases change.
- Project one-offs go in wrapper entrypoints that import the phases — never hooks in the base (docs "Escape hatch: wrapper entrypoints").
- Local `container/__pycache__` (cpython-313/314) and the `.codegraph` symlink are machine-local, untracked artifacts.

## External References

| Need | File |
| ----- | ----- |
| Agent contract + project extension guide | `docs/standard-agent.md` |
| Deployment guide (host prep, proxy, platforms) | `docs/deployment.md` |
| Production compose template | `templates/compose.prod.agent.yml` |
| Migration guides (Freya, Mimir) | `docs/standard-agent.md#migrations` |
| Spec schema golden example | `templates/spec.example.json` |
| Env contract (base vs project vars) | `templates/env.example` |
