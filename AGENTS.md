# Agent Instructions

## Commands

| Task | Command |
| ----- | ------- |
| All tests | `./make.sh test` |
| One module | `python3 -m unittest discover -s container -p "test_spec.py"` |
| agentctl (Go) tests | `./make.sh agentctl-test` |
| agentctl build/install | `./make.sh agentctl-build` / `./make.sh agentctl-install` |
| Run agentctl without installing | `./make.sh agentctl <command> [flags]` (forwards to `go run`) |
| Lint (ruff, ruff-format, hadolint, markdownlint, golangci-lint/depguard) | `./make.sh lint` |
| Image smoke (CI + local; podman or docker, `SMOKE_ENGINE` override) | `./make.sh smoke` |
| agentctl front-door e2e (init → deploy → destroy; `E2E_ENGINE` override) | `./make.sh agentctl-e2e` |
| Build image (date tag default) | `./make.sh build` |
| Push image | `AGENT_BASE_VERSION=YYYY.MM.DD[.N] ./make.sh push` — refuses implicit tags; same-day follow-up releases use the `.N` run suffix |
| Validate a spec (CI gate) | `docker run --rm --env-file .env <image> --validate-spec` |
| CLI contract (CI + local) | `python3 tests/contract_test.py <image>` — real-CLI drift gate + upgrade-path gate (warm-volume reboot from last release; `CONTRACT_UPGRADE=required` in CI, `auto` locally) |

## Structure

```text
cmd/         agentctl CLI — operator tool for downstream agent repos (scaffold first)
container/   Image contract: entrypoint.py (boot), spec.py (loader), seed_automations.py
             (cron reconciler), Dockerfile, colocated test_*.py (never shipped)
docs/        standard-agent.md — the whole agent contract + grow-agent/trade-agent migration guides
             (incl. "Model providers via LiteLLM")
tests/       e2e surface (image-side, python stdlib): smoke_test.py (real-image
             fixture boots + graceful-shutdown drain), agentctl_e2e.py (front
             door: init → doctor → deploy → health → stop/start → destroy
             volume semantics → dev overlay), contract_test.py + contract/
             (real-CLI drift gate: emitted-flag cross-check vs --help + clean
             shim-free boot + upgrade-path warm-volume reboot from the last
             published release), fixtures/* (grow-agent-like, trade-agent-like,
             litellm-like — boot-tested spec+automations trees; input for
             smoke; consult, don't copy whole), shim/openclaw (fake CLI;
             asserts via invocation log)
internal/    agentctl engine, layered by import direction:
             project + process (foundations: repo contract readers,
             Runner/engine policy — import nothing internal), compose
             (engine argv vocabulary), platform (deployment port +
             Deployment IR; adapters under platform/dockercompose and
             platform/fly — fly ships its own embedded fly.toml.tmpl and
             scaffolds deploy/fly.toml), cli (cobra composition root
             incl. the platform registry — the only package allowed to
             import adapters), scaffold (self-contained leaf owning its
             embedded tmpl/ tree)
scripts/     check-image-refs.sh only (release-time GHCR tag gate)
examples/    Image-contract examples: spec.example.json (golden), env.example,
             compose snippets (prod template incl. the LiteLLM sidecar),
             litellm/config.example.yaml (golden proxy config), workspace
             skeletons
```

## Where To Look

| Task | Location |
| ----- | -------- |
| Change boot behavior | `container/entrypoint.py` — each phase is a plain function over `(spec, env)` |
| Spec schema change | `container/spec.py` + `examples/spec.example.json` + `docs/standard-agent.md` (same commit) |
| Cron reconcile behavior | `container/seed_automations.py` |
| Graceful shutdown / drain behavior | `container/entrypoint.py` — `supervise`, `ShutdownSupervisor`, `parse_shutdown_grace` |
| Env var contract (base vs project) | `examples/env.example`, `docs/standard-agent.md#environment-contract` |
| LiteLLM sidecar (model/provider config home) | `docs/standard-agent.md#model-providers-via-litellm`, `examples/litellm/config.example.yaml`, scaffold `internal/scaffold/tmpl/litellm/` |
| Smoke failure | `logs/smoke-*.log` (kept on failure, deleted on success) + `tests/smoke_test.py` |
| Migration guides | `docs/standard-agent.md#migrations` |
| Scaffold a new downstream agent repo | `cmd/agentctl` — `go run ./cmd/agentctl init <dir>` |

## Code Map

Symbols relative to `container/`.

| Symbol | Type | Location | Role |
| ------ | ---- | -------- | ---- |
| `main` | fn | entrypoint.py:1678 | Phase orchestration; forks post_startup then `supervise()`s the CMD — returns its exit code after graceful-shutdown drain; other int returns: `--validate-spec` (0/1) and usage (2) |
| `supervise` / `ShutdownSupervisor` / `parse_shutdown_grace` | fn/cls | entrypoint.py:1653 / :1509 / :1490 | Graceful shutdown: CMD runs in its own process group; first SIGTERM/SIGINT forwards to the CMD pid only, the drain waits for the group to empty up to `AGENT_SHUTDOWN_GRACE` (default 600; 0 = forward + immediate force-kill), a second signal force-kills, an unprompted CMD exit kills the group (restart semantics); exit code = CMD's, 128+N when signaled |
| `backup_before_upgrade` | fn | entrypoint.py:1427 | Verified backup on `AGENT_BASE_VERSION` delta (warm volume); failure aborts — data safety beats availability for migrations |
| `load_agent_spec` | fn | entrypoint.py:153 | Fail-closed load; `AGENT_SPEC_PATH` override |
| `first_boot_setup` | fn | entrypoint.py:277 | One-time setup; gated on `openclaw.json` absent; snapshots base plugin installs to `{data}/agent-managed-plugins` |
| `reconcile_config` / `reconcile_mcp` / `reconcile_plugins` | fn | entrypoint.py:367 / :569 / :710 | Idempotent reconcile; warn-never-raise; config writes batch via `config set --batch-json` where possible (`config_set_batch`); seeds `plugins.allow` when unowned (`_seed_plugins_allow`); seeds `gateway.bind=lan` unless the spec owns the path; seeds `models.providers.litellm.baseUrl=http://litellm:4000` for `litellm-api-key` specs unless the spec owns the path; `features.gateway_auth` retires the legacy config pair instead of writing it (`_retire_legacy_gateway_auth_pair` — the env var is the gateway's active surface); MCP entries re-register on flag drift (args-digest marker under `{data}`), removal gated on `features.mcp_prune`, plugin prune on `features.plugin_prune` (ownership markers under `{data}`); MCP + plugin orphan reports are warn-only |
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
- Go module at the repo root (`agentctl`, spf13/cobra + go.yaml.in/yaml/v3 + BurntSushi/toml — no viper): operator CLI for downstream projects (scaffold + dev loop + release verbs via the platform port + secrets + validate). Release verbs (`deploy/status/logs/mcp/stop/start/destroy`) dispatch through the `Platform` interface (`internal/platform`); the registry is the explicit map in `internal/cli/registry.go` (composition root — the port package must not import its adapters; adapters live in `internal/platform/<name>`, dockercompose + fly so far). Config is typed and fail-closed: `.agentctl.yaml` top level accepts only `platform` + per-platform namespaces (`compose.engine`, `compose.gateway_port`, `fly.app`, `fly.region`); unknown keys abort with rename hints for the legacy flat names. Scaffold templates embedded in `internal/scaffold/tmpl` (init tree) and per-adapter (`fly` embeds its own fly.toml.tmpl). Dependency rule (machine-enforced by depguard via golangci-lint — pre-commit hook + `.golangci.yml`): `project`/`process` import nothing internal; `compose` imports only `process` + `project`; the `platform` port imports no adapter (project + process only); adapters (`internal/platform/<name>`) never import each other; only `internal/cli` imports adapters. Process execution flows through the `Runner` interface (`internal/process` — Run, RunOutput, LookPath); cobra-free logic lives in the foundation and port packages.
- Loader modules fail closed: unknown key/token, ambiguous shape → abort loudly; never a silent empty string or skip.
- Secrets flow only through `{env:VAR}` spec refs and env vars; resolved values must never reach logs — warnings name keys/env vars, never values (locked by SecretsCanary tests).
- `container/` files are the image contract; renaming/moving any of them changes downstream projects' Dockerfiles — update `docs/standard-agent.md` in the same commit. Modules import each other top-level (no package, no `__init__.py`); the Dockerfile COPYs exactly the three modules flat to `/opt/agent`.
- Reconcile failures warn and never raise (gateway availability > config completeness); loader failures abort the boot.
- Seeded automations run with a bounded tool allow-list (`seed_automations.DEFAULT_JOB_TOOLS` — fs/runtime/web/memory + `bundle-mcp`; recursion/spawn/browser excluded, OWASP ASI06). Per-job `tools:` header or spec `automations.default_tools` overrides; `*` = unrestricted. Per-job `model:` header overrides the global `automations.model` for that job (set on add, drift healed via `cron edit --model`). A `trigger-script:` header attaches a Gateway condition script (path inside the read-only `/opt/agent/scripts` sibling, content embedded at seed time; content drift healed via `cron edit --trigger-script`, removal via `--clear-trigger`) — the surface aborts seeding unless `AGENT_AUTOMATION_TRIGGERS=1` (which arms `cron.triggers.enabled`; evaluation runs with the owning agent's FULL tool policy, so the opt-in is deliberate). The base also sets `tools.deny` (cron, subagents, sessions_spawn, nodes) unless any env-active spec `tools.*` config entry exists (an `if_env` guard that never fires configures nothing).
- Formatter is `ruff-format` (not black) via pre-commit, alongside ruff, hadolint (`container/Dockerfile` only), markdownlint (`tests/fixtures/***` ignored).
- CI composes reusable actions from `tankdonut/github-actions` (`pre-commit`, `setup-python-uv`, `ghcr-login`); do not hand-roll equivalents. Exception: the multi-arch image jobs in `.github/workflows/ci.yml` use native per-arch runners + `imagetools` merge — the shared `build-and-publish` workflow hardcodes `ubuntu-latest` and cannot express per-arch builds (and a single multi-platform buildx push drops the HEALTHCHECK via the OCI exporter).
- Releases bump two Go constants on main before tagging: `internal/cli` `Version` and `internal/scaffold` `DefaultBaseTag`, both equal to the tag verbatim. The `release` job cross-compiles `agentctl-linux-amd64/arm64` + `agentctl-SHA256SUMS` into the release assets and fails if `agentctl version` ≠ tag (see the `releasing-agent-base` skill).
- Images publish under date tags only (`YYYY.MM.DD`); no `latest` exists; push requires explicit `AGENT_BASE_VERSION`.

## Anti-Patterns

- Baked default for `automations.model` — hard error naming `--model` + `AUTOMATION_MODEL`; defaults drift silently between agents sharing the image.
- Bind-mounting automations in any mode (incl. dev overlay) — writable cron prompts are an agent self-modification surface.
- Setting `OPENCLAW_HOME` — double-nests `{data}`; the entrypoint pops it at import.
- Writing docs to `{data}/docs` — the hard standard is `{data}/workspace/docs`.
- Floating image tags; hand-rolled CI steps.
- Editing `AGENTS.md` under `examples/workspace/` or `tests/fixtures/**/workspace/` as project docs — those are shipped agent personas (payload), not repo documentation.
- In tests: `from seed_automations import X` — reload discipline requires module-attribute access (`seed_automations.X`).

## Unique Styles

- `# allow: SIZE_OK` header marks contractually-single test files exempt from size ceilings.
- Meta-tests: import-safety classes assert importing never boots.
- Secrets canary tests: plant a canary, assert it reaches CLI argv but never captured stdout/stderr.
- Smoke asserts phase order by line number in the shim invocation log (`tests/shim/openclaw`), plus proof-of-absence checks (e.g. no `memory index` on clean status).

## Notes

- Smoke runs in CI (`smoke` job, docker via `SMOKE_ENGINE`) and locally (podman-first); logs are `logs/smoke-*.log` (gitignored) — kept on failure, removed on success.
- `tests/smoke_test.py` embeds a Python RUNNER mirroring `main()` minus the fork/supervise handoff — update both when phases change.
- Project one-offs go in wrapper entrypoints that import the phases — never hooks in the base (docs "Escape hatch: wrapper entrypoints").
- Local `container/__pycache__` (cpython-313/314) and the `.codegraph` symlink are machine-local, untracked artifacts.

## External References

| Need | File |
| ----- | ----- |
| Agent contract + project extension guide | `docs/standard-agent.md` |
| Deployment guide (host prep, proxy, platforms) | `docs/deployment.md` |
| Production compose template | `examples/compose.prod.agent.yml` |
| Migration guides (grow-agent, trade-agent) | `docs/standard-agent.md#migrations` |
| Spec schema golden example | `examples/spec.example.json` |
| Env contract (base vs project vars) | `examples/env.example` |
