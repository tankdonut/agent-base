# container/ — Image Contract

The three Python modules the Dockerfile COPYs flat to `/opt/agent`, plus their colocated
unittest suites. Repo-wide rules live in the root `AGENTS.md`; this file covers module
internals only.

## Files

| File | Role |
| ---- | ---- |
| `entrypoint.py` | Boot phases, each a plain function over `(spec, env)`, individually callable from wrapper entrypoints |
| `spec.py` | Fail-closed spec.json loader — library only (shebang is dead; no `__main__`) |
| `seed_automations.py` | Cron reconciler, dual-mode: imported in-process by entrypoint AND standalone CLI; jobs seeded with a bounded `--tools` allow-list (DEFAULT_JOB_TOOLS) unless a `tools:` header / `automations.default_tools` / `*` says otherwise; a per-job `model:` header overrides the global automation model (`payload.model`, healed via `cron edit --model`); a `trigger-script:` header embeds a condition script from the scripts dir sibling (`AGENT_SCRIPTS_DIR`, default `/opt/agent/scripts`, trimmed ≤64 KiB) whose content drift heals via `cron edit --trigger-script` and whose removal heals via `--clear-trigger` — gated on `AGENT_AUTOMATION_TRIGGERS=1`, which also arms `cron.triggers.enabled` before seeding |
| `Dockerfile` | `FROM ghcr.io/openclaw/openclaw:2026.7.1-2`; apt python3 (no pip — stdlib by construction) + gh CLI from the cli.github.com apt repo (Debian ships none; consumed by `authenticate_gh`); tini → entrypoint → `openclaw gateway`; `USER 1000:1000` (node); EXPOSE 18789 |
| `test_{spec,entrypoint,seed_automations}.py` | One suite per module; never shipped (explicit COPYs only) |

## Boot Phase → Function

| Phase | Function (entrypoint.py) | Gist |
| ----- | ------------------------ | ---- |
| Load | `load_agent_spec` :153 | fail-closed; `AGENT_SPEC_PATH` override |
| Upgrade backup | `backup_before_upgrade` :1427 | on `AGENT_BASE_VERSION` delta vs `{data}/last-image-version`, warm volume → `openclaw backup create --verify --output /backups` (CLI refuses inside `{data}`); failure aborts (exit 1, marker kept → retry); fresh volume records marker only |
| First boot | `first_boot_setup` :277 | only when `openclaw.json` absent; llama-cpp provider install is base behavior, not spec-configurable; a failed `setup` aborts the boot cleanly (exit 1, named env var) — `zai-coding-*` auth is load-gated on `ZAI_API_KEY` (see `spec.required_env_for_auth_choice`); ends with `_snapshot_base_plugins` → `{data}/agent-managed-plugins` (ownership for the orphan report AND the plugins.allow seed) |
| Reconcile | `reconcile_config` :367 → `reconcile_mcp` :569 → `reconcile_plugins` :710 | idempotent; `config_set` shells out only on drift (compares `openclaw.json` directly), `config_set_batch` applies multi-key deltas in one `--batch-json` call; `_seed_plugins_allow` seeds the allowlist when neither config nor an env-active spec entry owns it; `features.gateway_auth` arms auth via the env var natively and `_retire_legacy_gateway_auth_pair` unsets the legacy pair (exact-value match only); MCP entries re-register on resolved-flag drift (args-digest marker `{data}/agent-mcp-args`; hand-registered or pre-marker servers converge once; digests only, never resolved values; remote `transport` participates in the digest); MCP removal is gated on `features.mcp_prune` (ownership-marked via `{data}/agent-managed-mcp`; if_env-skipped entries count as still spec'd); MCP orphan report is warn-only; plugin orphan report is warn-only and disabled without the plugins marker |
| gh auth | `authenticate_gh` :835 | every boot when `features.gh_auth`; non-fatal |
| Seed | `seed_content` :884 | workspace first boot only; skills + docs full replace every boot; `AGENT_SKIP_SEED=1` skips seeding only |
| Post-startup | `post_startup` :1295 | forked child: gateway wait ≤180s, cron seed in-process, memory reindex (3 tries / 10s backoff; degraded success is retryable), STABLE doctor skills reconcile (`disable_unavailable_skills` — the check only sees enabled skills, so disables need the finding in two settle-spaced doctor runs and heals are proven by re-enable + re-check; failed heals re-disable in-boot and defer retry until the image changes via `{data}/doctor-heal-attempts`; writes batched via `--batch-json`; steady-state boots write nothing and run doctor once; `{data}/doctor-disabled-skills` marker preserves operator intent), security audit + report persistence (`{data}/logs/{doctor,security}-report.json` on findings with up-to-ten per-finding `checkId + path` log lines; warn-only, never gates), boot summary `{data}/status.json` (image version + warning count — text never reaches disk) |
| Supervise | `supervise` :1653 | parent phase: CMD spawns via `Popen(start_new_session=True)` in its own process group; first SIGTERM/SIGINT forwards to the CMD pid only, the drain waits for the group to empty (killpg probe — orphans re-parent to tini) up to `AGENT_SHUTDOWN_GRACE`, then group SIGKILL; a second signal force-kills at once; an unprompted CMD exit kills the group (exec-era teardown semantics, restart stays prompt); returns the CMD's exit code (128+N when signaled) |
| CI gate | `validate_spec` :1660 | dry parse of spec + automations; zero mutation |

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
