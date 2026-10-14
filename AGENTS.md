# Agent Instructions

## Commands

| Task | Command |
| ----- | ------- |
| All tests | `./make.sh test` |
| One module | `python3 -m unittest discover -s container -p "test_spec.py"` |
| Lint (ruff, ruff-format, hadolint, markdownlint, check-yaml + env-example contract) | `./make.sh lint` |
| Image smoke (CI + local; podman or docker, `SMOKE_ENGINE` override) | `./make.sh smoke` |
| Build image (date tag default) | `./make.sh build` |
| Push image | `AGENT_BASE_VERSION=YYYY.MM.DD[.N] ./make.sh push` — refuses implicit tags; same-day follow-up releases use the `.N` run suffix |
| Validate a spec (CI gate) | `docker run --rm --env-file .env <image> --validate-spec` |
| CLI contract (CI + local) | `python3 tests/contract_test.py <image>` — real-CLI drift gate + upgrade-path gate (warm-volume reboot from last release; `CONTRACT_UPGRADE=required` in CI, `auto` locally) |

## Structure

```text
container/   Image contract: entrypoint.py (boot), spec.py (loader), seed_automations.py
             (cron reconciler), Dockerfile, colocated test_*.py (never shipped)
docs/        standard-agent.md — the whole agent contract (incl. "Model providers
             via LiteLLM"); deployment.md — host prep, proxy, platforms
tests/       e2e surface (image-side, python stdlib): smoke_test.py (real-image
             fixture boots + graceful-shutdown drain), contract_test.py + contract/
             (real-CLI drift gate: emitted-flag cross-check vs --help + clean
             shim-free boot + upgrade-path warm-volume reboot from the last
             published release), fixtures/* (grow-agent-like, trade-agent-like,
             litellm-like — boot-tested spec+automations trees; input for
             smoke; consult, don't copy whole), shim/openclaw (fake CLI;
             asserts via invocation log)
scripts/     check-image-refs.sh (release-time GHCR tag gate)
examples/    Image-contract examples: spec.example.json (golden), env.example,
             compose snippets (prod template incl. the LiteLLM sidecar),
             litellm/config.example.yaml (golden proxy config), workspace
             skeletons
VERSION      Release intent: the date tag the next release publishes
```

## Where To Look

| Task | Location |
| ----- | -------- |
| Change boot behavior | `container/entrypoint.py` — each phase is a plain function over `(spec, env)` |
| Spec schema change | `container/spec.py` + `examples/spec.example.json` + `docs/standard-agent.md` (same commit) |
| Cron reconcile behavior | `container/seed_automations.py` |
| Graceful shutdown / drain behavior | `container/entrypoint.py` — `supervise`, `ShutdownSupervisor`, `parse_shutdown_grace` |
| Env var contract (base vs project) | `examples/env.example`, `docs/standard-agent.md#environment-contract` |
| LiteLLM sidecar (model/provider config home) | `docs/standard-agent.md#model-providers-via-litellm`, `examples/litellm/config.example.yaml`, `examples/compose.prod.agent.yml` |
| Smoke failure | `logs/smoke-*.log` (kept on failure, deleted on success) + `tests/smoke_test.py` |

## Code Map

Symbols relative to `container/`.

| Symbol | Type | Location | Role |
| ------ | ---- | -------- | ---- |
| `main` | fn | entrypoint.py:1678 | Phase orchestration; forks post_startup then `supervise()`s the CMD — returns its exit code after graceful-shutdown drain; other int returns: `--validate-spec` (0/1) and usage (2) |
| `supervise` / `ShutdownSupervisor` / `parse_shutdown_grace` | fn/cls | entrypoint.py:1653 / :1509 / :1490 | Graceful shutdown: CMD runs in its own process group; first SIGTERM/SIGINT forwards to the CMD pid only, the drain waits for the group to empty up to `AGENT_SHUTDOWN_GRACE` (default 600; 0 = forward + immediate force-kill), a second signal force-kills, an unprompted CMD exit kills the group (restart semantics); exit code = CMD's, 128+N when signaled |
| `backup_before_upgrade` | fn | entrypoint.py:1566 | Upgrade prep on `AGENT_BASE_VERSION` delta (warm volume): raw state-db snapshot → `openclaw doctor --fix --non-interactive` (state-db schema migrations; the 2026.10+ CLI gates backup + gateway on them) → `openclaw backup create --verify`; failure aborts — data safety beats availability for migrations |
| `load_agent_spec` | fn | entrypoint.py:153 | Fail-closed load; `AGENT_SPEC_PATH` override |
| `first_boot_setup` | fn | entrypoint.py:277 | One-time setup; gated on `openclaw.json` absent; snapshots base plugin installs to `{data}/agent-managed-plugins` |
| `reconcile_config` / `reconcile_mcp` / `reconcile_plugins` | fn | entrypoint.py:367 / :569 / :710 | Idempotent reconcile; warn-never-raise; config writes batch via `config set --batch-json` where possible (`config_set_batch`); seeds `plugins.allow` when unowned (`_seed_plugins_allow`); seeds `gateway.bind=lan` unless the spec owns the path; seeds `models.providers.litellm.baseUrl=http://litellm:4000` for `litellm-api-key` specs unless the spec owns the path; merges spec-referenced models (model.fallback, automations.model) + the GLM 5.3 series (`zai-coding-*` specs) into an existing `agents.defaults.models` allowlist — never creates one (absent = allow-any) and stands down when a spec entry owns the path (`_seed_agents_default_models`); `features.gateway_auth` retires the legacy config pair instead of writing it (`_retire_legacy_gateway_auth_pair` — the env var is the gateway's active surface); MCP entries re-register on flag drift (args-digest marker under `{data}`), removal gated on `features.mcp_prune`, plugin prune on `features.plugin_prune` (ownership markers under `{data}`); MCP + plugin orphan reports are warn-only |
| `authenticate_gh` | fn | entrypoint.py:835 | gh auth from `AGENT_GIT_TOKEN`; every boot, non-fatal |
| `seed_content` | fn | entrypoint.py:884 | workspace first boot only; skills + docs full replace every boot |
| `post_startup` | fn | entrypoint.py:1295 | Forked child: gateway wait ≤180s, cron seed in-process, memory reindex, stable doctor skills reconcile (`disable_unavailable_skills` — two-run confirmed, heals proven by re-enable + re-check, batched writes, heal retries deferred until image change via `{data}/doctor-heal-attempts`), diagnostics with per-finding detail lines (doctor/security reports to `{data}/logs`, boot summary `{data}/status.json`) |
| `load_spec` | fn | spec.py:527 | Strict v1 loader; errors carry the JSON path |
| `Spec` / `SpecError` | cls | spec.py | Frozen spec / `ValueError` subtype |
| `LocalMcpServer` / `RemoteMcpServer` | cls | spec.py:114 / :132 | stdio vs HTTP MCP; exactly one of `command` / `url` |
| `build_jobs` | fn | seed_automations.py | Parse `automations/*.md` fail-closed |
| `reconcile` | fn | seed_automations.py | Idempotent cron add/edit; heals drift with one edit |

## Key Conventions

- Python 3.11 floor (bookworm image), stdlib only; no 3.12+ syntax; full type annotations (convention — no mypy gate); `unittest` + `mock`, never pytest. Stdlib-only is enforced by construction: the image installs no pip; `.ruff.toml` targets py311, line 100; CI matrix is 3.11 (floor) + 3.14 (drift guard).
- The repo ships one artifact: the agent-base image. Release intent lives in the root `VERSION` file (date tag `YYYY.MM.DD[.N]`): a main push whose `VERSION` names an unreleased tag qualifies a candidate in the private `ghcr.io/tankdonut/agent-base-staging` package (per-arch builds, index assembly, full gate matrix against the exact index digest, SLSA attestations, eligibility receipt); the tag push PROMOTES — receipt-gated `oras cp --recursive` of the qualified digest to the public tag with an `I_final == I_qualified` identity assert, then tags the copied per-arch children `<tag>-amd64`/`<tag>-arm64` (cosmetic `continue-on-error` step — readable package rows instead of GHCR's `sha256-<digest>` names); the release job never rebuilds (see the `releasing-agent-base` skill).
- Loader modules fail closed: unknown key/token, ambiguous shape → abort loudly; never a silent empty string or skip.
- Secrets flow only through `{env:VAR}` spec refs and env vars; resolved values must never reach logs — warnings name keys/env vars, never values (locked by SecretsCanary tests).
- `container/` files are the image contract; renaming/moving any of them changes downstream projects' Dockerfiles — update `docs/standard-agent.md` in the same commit. Modules import each other top-level (no package, no `__init__.py`); the Dockerfile COPYs exactly the three modules flat to `/opt/agent`.
- First-boot plugin installs try the plugin build matching the shipped openclaw runtime version before the unpinned name (`_install_embedding_provider` in `container/entrypoint.py` — registry "latest" can require a newer plugin API than the runtime exposes and break fresh boots); bumping the `container/Dockerfile` openclaw base needs no plugin change, and the contract job's real boot catches bad pairings.
- Reconcile failures warn and never raise (gateway availability > config completeness); loader failures abort the boot.
- Seeded automations run with a bounded tool allow-list (`seed_automations.DEFAULT_JOB_TOOLS` — fs/runtime/web/memory + `bundle-mcp`; recursion/spawn/browser excluded, OWASP ASI06). Per-job `tools:` header or spec `automations.default_tools` overrides; `*` = unrestricted. Per-job `model:` header overrides the global `automations.model` for that job (set on add, drift healed via `cron edit --model`). A `trigger-script:` header attaches a Gateway condition script (path inside the read-only `/opt/agent/scripts` sibling, content embedded at seed time; content drift healed via `cron edit --trigger-script`, removal via `--clear-trigger`) — the surface aborts seeding unless `AGENT_AUTOMATION_TRIGGERS=1` (which arms `cron.triggers.enabled`; evaluation runs with the owning agent's FULL tool policy, so the opt-in is deliberate). The base also sets `tools.deny` (cron, automations, subagents, sessions_spawn, nodes — `automations` is the scheduler tool's name since openclaw 2026.8) unless any env-active spec `tools.*` config entry exists (an `if_env` guard that never fires configures nothing).
- Formatter is `ruff-format` (not black) via pre-commit, alongside ruff, hadolint (`container/Dockerfile` only), markdownlint (`tests/fixtures/***` ignored).
- CI composes reusable actions from `tankdonut/github-actions` (`pre-commit`, `setup-python-uv`, `ghcr-login`); do not hand-roll equivalents. Exception: the multi-arch image jobs in `.github/workflows/ci.yml` use native per-arch runners + `imagetools` merge — the shared `build-and-publish` workflow hardcodes `ubuntu-latest` and cannot express per-arch builds (and a single multi-platform buildx push drops the HEALTHCHECK via the OCI exporter).
- Images publish under date tags only (`YYYY.MM.DD`); no `latest` exists; push requires explicit `AGENT_BASE_VERSION`. Promotion also tags per-arch companions (`<tag>-amd64`/`<tag>-arm64`) pointing into the promoted index — immutable like the date tags.

## Anti-Patterns

- Baked default for `automations.model` — hard error naming `--model` + `AUTOMATION_MODEL`; defaults drift silently between agents sharing the image.
- Bind-mounting automations in any mode — writable cron prompts are an agent self-modification surface (and there is no overlay: content is image-baked in every mode).
- Setting `OPENCLAW_HOME` — double-nests `{data}`; the entrypoint pops it at import.
- Writing docs to `{data}/docs` — the hard standard is `{data}/workspace/docs`.
- Editing `AGENTS.md` under `examples/workspace/` or `tests/fixtures/**/workspace/` as project docs — those are shipped agent personas (payload), not repo documentation.
- Floating image tags; hand-rolled CI steps.
- In tests: `from seed_automations import X` — reload discipline requires module-attribute access (`seed_automations.X`).

## Unique Styles

- `# allow: SIZE_OK` header marks contractually-single test files exempt from size ceilings.
- Meta-tests: import-safety classes assert importing never boots.
- Secrets canary tests: plant a canary, assert it reaches CLI argv but never captured stdout/stderr.
- Smoke asserts phase order by line number in the shim invocation log (`tests/shim/openclaw`), plus proof-of-absence checks (e.g. no `memory index` on clean status).

## Notes

- Workflow evidence classes are never conflated: PR gates certify the **tree** (SHA correctness); main-push **candidate** gates certify the **exact artifact** (image digest identity); the nightly run is only the published-image watchdog (boots the newest public date tag, asserts baked-version identity + HEALTHCHECK). Harnesses accept explicit candidate inputs — `SMOKE_IMAGE` (smoke), `CONTRACT_BASELINE_IMAGE` (contract stage D). An explicit override is a required identity: unavailable must fail the run, never silently test different bytes; only the ambient defaults stay best-effort (skip/build-if-absent).
- Smoke runs in CI (`smoke` job, docker via `SMOKE_ENGINE`) and locally (podman-first); logs are `logs/smoke-*.log` (gitignored) — kept on failure, removed on success.
- `tests/smoke_test.py` embeds a Python RUNNER mirroring `main()` minus the fork/supervise handoff — update both when phases change.
- The example pair is gated: pre-commit `check-yaml` parses the compose + litellm examples, and `container/test_spec.py` `EnvExampleContract` pins `examples/env.example` against `examples/spec.example.json` (missing/drifted `{env:}` refs, `if_env` guards, and golden-env keys fail the suite).
- Project one-offs go in wrapper entrypoints that import the phases — never hooks in the base (docs "Escape hatch: wrapper entrypoints").
- Local `container/__pycache__` (cpython-313/314) and the `.codegraph` symlink are machine-local, untracked artifacts.

## External References

| Need | File |
| ----- | ---- |
| Agent contract + project extension guide | `docs/standard-agent.md` |
| Deployment guide (host prep, proxy, platforms) | `docs/deployment.md` |
| Production compose template | `examples/compose.prod.agent.yml` |
| Spec schema golden example | `examples/spec.example.json` |
| Env contract (base vs project vars) | `examples/env.example` |
| Release procedure | `skills/releasing-agent-base/SKILL.md` |
