# container/ — Image Contract

The three Python modules the Dockerfile COPYs flat to `/opt/agent`, plus their colocated
unittest suites. Repo-wide rules live in the root `AGENTS.md`; this file covers module
internals only.

## Files

| File | Role |
| ---- | ---- |
| `entrypoint.py` | Boot phases, each a plain function over `(spec, env)`, individually callable from wrapper entrypoints |
| `spec.py` | Fail-closed spec.json loader — library only (shebang is dead; no `__main__`) |
| `seed_automations.py` | Cron reconciler, dual-mode: imported in-process by entrypoint AND standalone CLI; jobs seeded with a bounded `--tools` allow-list (DEFAULT_JOB_TOOLS) unless a `tools:` header / `automations.default_tools` / `*` says otherwise |
| `Dockerfile` | `FROM ghcr.io/openclaw/openclaw:2026.7.1-2`; apt python3 (no pip — stdlib by construction) + gh CLI from the cli.github.com apt repo (Debian ships none; consumed by `authenticate_gh`); tini → entrypoint → `openclaw gateway`; `USER 1000:1000` (node); EXPOSE 18789 |
| `test_{spec,entrypoint,seed_automations}.py` | One suite per module; never shipped (explicit COPYs only) |

## Boot Phase → Function

| Phase | Function (entrypoint.py) | Gist |
| ----- | ------------------------ | ---- |
| Load | `load_agent_spec` :125 | fail-closed; `AGENT_SPEC_PATH` override |
| Upgrade backup | `backup_before_upgrade` :792 | on `AGENT_BASE_VERSION` delta vs `{data}/last-image-version`, warm volume → `openclaw backup create --verify --output /backups` (CLI refuses inside `{data}`); failure aborts (exit 1, marker kept → retry); fresh volume records marker only |
| First boot | `first_boot_setup` :222 | only when `openclaw.json` absent; llama-cpp provider install is base behavior, not spec-configurable; a failed `setup` aborts the boot cleanly (exit 1, named env var) — `zai-coding-*` auth is load-gated on `ZAI_API_KEY` (see `spec.required_env_for_auth_choice`); ends with `_snapshot_base_plugins` → `{data}/agent-managed-plugins` (ownership for the orphan report) |
| Reconcile | `reconcile_config` :302 → `reconcile_mcp` :333 → `reconcile_plugins` :402 | idempotent; `config_set` shells out only on drift (compares `openclaw.json` directly); MCP removal is gated on `features.mcp_prune` (ownership-marked via `{data}/agent-managed-mcp`; if_env-skipped entries count as still spec'd); MCP orphan report is warn-only; plugin orphan report is warn-only and disabled without the plugins marker |
| gh auth | `authenticate_gh` :464 | every boot when `features.gh_auth`; non-fatal |
| Seed | `seed_content` :502 | workspace first boot only; skills + docs full replace every boot; `AGENT_SKIP_SEED=1` skips seeding only |
| Post-startup | `post_startup` :1016 | forked child: gateway wait ≤180s, cron seed in-process, memory reindex (3 tries / 10s backoff; degraded success is retryable), doctor skills reconcile (`disable_unavailable_skills` — disable flagged, re-enable healed via `{data}/doctor-disabled-skills` marker; stdout is authoritative because doctor exits 1 iff findings exist; ONE shared doctor run feeds the diagnostics too), security audit + report persistence (`{data}/logs/{doctor,security}-report.json` on findings; warn-only, never gates), boot summary `{data}/status.json` (image version + warning count — text never reaches disk) |
| Supervise | `supervise` :1350 | parent phase: CMD spawns via `Popen(start_new_session=True)` in its own process group; first SIGTERM/SIGINT forwards to the CMD pid only, the drain waits for the group to empty (killpg probe — orphans re-parent to tini) up to `AGENT_SHUTDOWN_GRACE`, then group SIGKILL; a second signal force-kills at once; an unprompted CMD exit kills the group (exec-era teardown semantics, restart stays prompt); returns the CMD's exit code (128+N when signaled) |
| CI gate | `validate_spec` :848 | dry parse of spec + automations; zero mutation |

## Conventions

- Top-level imports only (`from spec import ...`) — no package, no `__init__.py`. Works because the Dockerfile puts all three modules in one dir and `unittest discover -s container` puts the dir on `sys.path`.
- Import-time side effects: `entrypoint` pops `OPENCLAW_HOME` (double-nest guard, meta-tested); `seed_automations` reads `TELEGRAM_CHAT_ID`. Tests reload with `importlib.reload` and access `seed_automations.X` — never from-imports (stale after reload).
- In-process cron seeding: entrypoint calls `seed_automations.main([...])` and catches `SystemExit`.
- `spec.py` never touches `os.environ` — env is the passed `Mapping` only; `{env:NAME}` / `{data}` templating resolves eagerly, single pass, never re-scans replacement text.
- Reconcile failures warn (naming the key/server/plugin, never values) and never raise; loader failures abort the boot.
- `data_dir()` is resolved per call, never cached (HOME overrides must hold for the whole boot); `openclaw.json` is re-read on every `config_set` because mid-reconcile CLI calls mutate it.

## Anti-Patterns

- Silent skip, best-effort parse, or empty-string fallback — every violation aborts with the JSON path / env var name.
- Default for `automations.model` — hard error naming `--model` + `AUTOMATION_MODEL`.
- Splitting a module or adding a fourth file (`spec.py:26-28` forbids it).
- Logging values that may carry secrets — keys/env names only; SecretsCanary tests lock this.
- Caching `data_dir()` or the `openclaw.json` snapshot across reconcile.
- Expecting the drain to see `setsid`-escaped descendants — only the CMD's process-group members are drained; signaling the whole group on the first TERM (automations must outlive the gateway's signal).
