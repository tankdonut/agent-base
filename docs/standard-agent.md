# Standard Agent Contract

`agent-base` is the shared OpenClaw agent image for tankdonut projects. One
image carries the whole boot pipeline: spec validation, first-boot setup,
config/MCP/plugin reconciliation, content seeding, cron scheduling, and
memory indexing. A project contributes no boot code of its own. It ships a
thin Dockerfile that drops its content into the image (a `spec.json`, three
seed directories, and an automations directory) and inherits everything
else.

The image distills two agents that already ran this pipeline as bespoke
scripts:

- **Freya** (grow-agent): the value-compared config fast path, first-boot
  channel setup, cron seeding, memory reindex with retry and degraded-mode
  detection, and the GitHub-auth bootstrap.
- **Mimir** (trade-agent): remote MCP servers with env-templated URLs and
  headers, the six-server registration matrix, and docs as image-baked
  reference content replaced every boot.

Their `FREYA_*` / `MIMIR_*` environment names are gone. One `AGENT_*`
vocabulary covers both, declared per project in `spec.json`.

This document is the contract. The code in `container/` is its
implementation: `spec.py` (loader), `entrypoint.py` (boot phases),
`seed_automations.py` (cron reconciler). Worked specs live in
`templates/spec.example.json` (golden example) and `fixtures/freya-like/`
plus `fixtures/mimir-like/` (real boot-tested fixtures; consult them rather
than copying them whole).

## Quick start

A project image is a FROM line and five COPY lines. The base image already
carries the entrypoint chain (tini → `entrypoint.py` → the container CMD,
`openclaw gateway`) and the Python runtime; your Dockerfile inherits all of
it and adds only content:

```dockerfile
FROM ghcr.io/tankdonut/agent-base:2026.08.21

# The declarative boot contract (container/spec.py loads it fail-closed).
COPY --chown=node:node spec.json /opt/agent/spec.json

# Seed content: workspace first boot, skills + docs every boot.
COPY --chown=node:node workspace/ /opt/seed/workspace/
COPY --chown=node:node skills/ /opt/seed/skills/
COPY --chown=node:node docs/ /opt/seed/docs/

# Cron specs: image-baked, never host-mounted.
COPY --chown=node:node automations/ /opt/agent/automations/
```

Validate the spec and automations in CI without touching a volume:

```sh
docker run --rm --env-file .env <image> --validate-spec
```

Exit 0 means both parsed; exit 1 prints the reason on stderr. The gateway
never starts in this mode and nothing is mutated.

To use the GitHub CLI from agent sessions, enable it in the spec and pass
the token (a classic PAT with `repo` scope is the reliable choice):

```json
"features": { "gh_auth": true }
```

```text
AGENT_GIT_TOKEN=ghp_...
```

The entrypoint runs `gh auth login --with-token` on every boot when
`features.gh_auth` is true (gh state lives outside the data volume, so it
does not survive restarts). The base image does not ship the `gh` binary;
a project that enables `gh_auth` installs it in its own Dockerfile.
Failure is non-fatal and the token is never logged.

Wire the runtime up with `templates/compose.agent.yml` (service snippet)
and `templates/compose.dev.agent.yml` (hot-reload overlay), then fill
`.env` from `templates/env.example`.

## Environment contract

The base image reads only the variables below. Everything else in a
project's `.env` is spec-dependent: it reaches the runtime solely through
`spec.json` `{env:NAME}` refs, `if_env` guards, and `split_csv` values, so
its names are the project's choice. `templates/env.example` states the same
split with copy-paste entries.

| Variable | Default | Effect |
| --- | --- | --- |
| `AGENT_SPEC_PATH` | `/opt/agent/spec.json` | Override the spec location (tests, fixtures, wrapper entrypoints). |
| `AGENT_MANAGE_CONFIG` | `1` | `0` skips config/MCP/plugin reconciliation for operators who manage `openclaw.json` manually. |
| `AGENT_SKIP_SEED` | `0` | `1` skips content seeding only. Reconciliation still runs; dev overlays bind-mount the content instead. |
| `AGENT_MEMORY_REINDEX` | `1` | `0` skips the post-startup memory reindex. |
| `AGENT_GIT_TOKEN` | unset | gh token, used only when `features.gh_auth` is true. Never logged. |
| `AGENT_AUTOMATIONS_DIR` | `/opt/agent/automations` | Override the automations directory. |
| `AUTOMATION_MODEL` | unset | Cron model fallback when `--model` is not passed. The entrypoint always passes `spec.automations.model`, so this matters only for manual `seed_automations.py` runs. |
| `TELEGRAM_CHAT_ID` | unset | Chat ID for cron delivery (base-standardized). Unset means jobs run without Telegram delivery. |

Do not set `OPENCLAW_HOME`. The entrypoint pops it at import: OpenClaw
treats it as a home directory and appends `.openclaw/` inside it, which
double-nests the data dir. The default (`~/.openclaw`, i.e.
`/home/node/.openclaw` in the container) is correct.

Resolved secret values never reach logs. Reconciliation warnings name the
config key that failed, never the value that was being set.

## spec.json reference

Schema v1. Loading is strict and fail-closed: an unknown key at any nesting
level aborts the boot, there are no warnings and no best-effort parsing,
and every error message starts with the JSON path of the offending node
(`mcp_servers[2].url: ...`). The full key set:

| Section | Keys | Notes |
| --- | --- | --- |
| `specVersion` | (none) | Must be `1`. Anything else is rejected before any other check. |
| `agent` | `name` | Required, non-empty. Reaches logs and seed messages. |
| `setup` | `auth_choice` | Required. Passed to `openclaw setup --auth-choice` on first boot (e.g. `zai-coding-global`). |
| `model` | `fallback` | Required. Registered via `openclaw models fallbacks add` on first boot. |
| `automations` | `model` | Required. Model for cron agent turns. No default exists by design (see decisions below). |
| `config` | `path`, `value`, `strict`, `if_env`, `split_csv` | `path` and `value` required; the rest are optional booleans / string lists. Applied in spec order. |
| `channels` | `type`, `use_env` | `type` required. `use_env` (default `true`) feeds the channel credentials from the environment. |
| `mcp_servers` | `name`, `command` or `url`, `args`, `env`, `headers`, `no_probe`, `timeout`, `if_env` | Exactly one of `command` (local stdio) and `url` (remote HTTP); specifying both or neither is an error. |
| `plugins` | `name`, `source` | `source` absent means install `name` from the registry; present means a local plugin directory and must be an absolute path. |
| `features` | `gh_auth` | Default `false`. See Quick start. |

### Templating

String values at any depth resolve two tokens at load time, in a single
pass (replacement text is never re-scanned):

- `{env:NAME}`: substituted from the boot environment. A missing `NAME` is
  a load error naming the variable.
- `{data}`: the live data root, `~/.openclaw` (`/home/node/.openclaw`).

Any other brace content, or an unclosed brace, is an error. Template
resolution happens once, eagerly, against the environment the loader was
given; nothing re-resolves later.

One consequence deserves calling out: `if_env` guards runtime application,
not load-time resolution. A spec that templates `{env:TELEGRAM_CHAT_ID}`
inside an `if_env: ["TELEGRAM_CHAT_ID"]` entry still requires the variable
at load time; a boot without it aborts loudly rather than running
half-configured. Pair `if_env` with literal values you want applied only
when some other variable exists (the freya-like fixture's
`heartbeat.target: telegram` entry shows the pattern).

### config entries

Each entry becomes one `openclaw config set path value` call, marshalled
from the resolved value:

- Plain string, no flags: passed through unquoted.
- `strict: true`, or any non-string JSON value: passed as `--strict-json`
  (the CLI stores real booleans, numbers, arrays, objects).
- `split_csv: true`: the resolved value must be a string. It is split on
  commas, items are stripped, empties are dropped, and the surviving list
  is set as strict JSON. This is how a comma-separated env var such as
  `TELEGRAM_ALLOWED_USERS=123,456` becomes a real array config value. An
  empty result is a load error naming the referenced env var(s); a
  configured CSV that yields nothing is a misconfiguration, not a skip.
- `if_env`: the entry is applied only when every listed variable is
  present. An unsatisfied guard is a logged skip, never an error.

On warm volumes the entrypoint compares each desired value against
`openclaw.json` directly and shells out only for keys that actually drift,
so a rebooting container costs one JSON read, not N CLI spawns.

### mcp_servers entries

Local (stdio) servers run a command; remote (HTTP) servers point at a URL.
Both reconcile idempotently: the entrypoint lists registered servers and
adds only the missing. `no_probe` defaults to `true` (skip the startup
probe); set `false` when you want registration to verify connectivity.
`timeout` (local only, seconds) caps the startup probe. `if_env` skips the
server when a listed variable is absent, which is the standard way to make
an optional API-keyed server conditional.

### Worked examples

- `templates/spec.example.json`: one entry per feature, annotated by shape.
- `fixtures/freya-like/spec.json`: local stdio servers with `--env` pairs,
  `split_csv` allowFrom, heartbeat entries behind `if_env`, a local plugin,
  `gh_auth` enabled.
- `fixtures/mimir-like/spec.json`: six servers mixing remote URLs
  (one key-templated query param, one bearer-token header) with local npx
  commands, `gh_auth` disabled, a different automation model.

## Seeded content and lifecycle

The base image bakes seed content under `/opt/seed/` and automations under
`/opt/agent/automations/`. At boot the entrypoint seeds the live data dir
(`{data}` = `/home/node/.openclaw`):

| Image source | Live destination | When | Policy |
| --- | --- | --- | --- |
| `/opt/seed/workspace/` | `{data}/workspace/` | First boot only | Copied when absent. The agent owns and evolves it afterwards. |
| `/opt/seed/skills/` | `{data}/skills/` | Every boot | Full replacement; image-baked skills must track the image. |
| `/opt/seed/docs/` | `{data}/workspace/docs/` | Every boot | Full replacement. Docs are reference content, not agent state. |
| (created) | `{data}/workspace/journal/` | Every boot | `mkdir -p`; the agent's append-only territory. |
| `/opt/agent/automations/` | cron jobs (via `openclaw cron`) | Every boot, post-startup | Reconciled idempotently; markdown never copied anywhere. |

Docs living at `{data}/workspace/docs` is a hard standard: there is no
`{data}/docs` destination, and the entrypoint never writes one. Agents
migrating from a legacy `{data}/docs` layout move once, in their wrapper
entrypoint (see below), before `seed_content` runs.

`AGENT_SKIP_SEED=1` skips content seeding only. Reconciliation still runs.
The dev overlay relies on exactly this: it bind-mounts `workspace/`,
`skills/`, and `docs/` over `/opt/seed/*` and sets `AGENT_SKIP_SEED=1`, so
the bind mounts are the seed and edits reach the next session without a
rebuild. Automations are never bind-mounted in any mode; writable cron
prompt files would be a self-modification surface for the agent.

## Boot sequence

`main()` runs the phases in order, then forks. Every phase is a plain
function in `container/entrypoint.py` and is individually callable from a
wrapper entrypoint.

1. **Load spec** (`load_agent_spec`): parse and template-resolve
   `spec.json` against the environment. Any violation aborts the boot
   before a single side effect.
2. **First boot** (`first_boot_setup`, only when `{data}/openclaw.json` is
   absent): `openclaw setup --auth-choice <setup.auth_choice>`, register
   `model.fallback`, add each channel, install
   `@openclaw/llama-cpp-provider` (key-free local embeddings for memory
   search; this one is base-image behaviour, not spec-configurable).
3. **Reconcile** (when `AGENT_MANAGE_CONFIG=1`): `reconcile_config`, then
   `reconcile_mcp`, then `reconcile_plugins`, as described above. All
   idempotent; failures warn and never raise, so a flaky registration cannot
   take the gateway down.
4. **gh auth** (`authenticate_gh`, only when `features.gh_auth` is true):
   `gh auth login --with-token` from `AGENT_GIT_TOKEN`.
5. **Seed** (`seed_content`): the table above, unless
   `AGENT_SKIP_SEED=1`.
6. **Fork.** The parent `os.execvp`s the container CMD (`openclaw
   gateway`) under tini. The child runs `post_startup` and then exits:

   - Wait for gateway health (polls `openclaw health` for up to 180s;
     timeout skips the rest, non-fatal).
   - Cron seeding: `seed_automations.main(["--model", <automations.model>])`
     in-process. Fail-closed (a bad or missing spec aborts before any
     mutation) and drift-healing (see decisions below).
   - Memory reindex (unless `AGENT_MEMORY_REINDEX=0`): clear stale
     reindex locks, check status, then full rebuild, incremental pass, or
     skip. Three attempts with 10s backoff; a degraded success
     (`chunks_vec not updated` on stderr, vectors skipped) counts as
     retryable because it is the memory-search-offline case.
   - Disable skills flagged by `openclaw doctor --lint` as not ready.
   - `openclaw config validate` and `openclaw doctor --lint`, warn-only:
     findings surface in logs, the gateway keeps running.

`--validate-spec` replaces all of the above with a dry parse (spec plus
automations directory) for CI.

## Standardization decisions

| Decision | Why |
| --- | --- |
| `AGENT_*` prefix replaces `FREYA_*` / `MIMIR_*` | One vocabulary across projects; the base cannot accidentally special-case one agent's names. |
| `TELEGRAM_CHAT_ID` is base-standard | Cron delivery needs one chat target the reconciler can read directly. All other `TELEGRAM_*` names stay project-side in spec refs (`TELEGRAM_BOT_TOKEN`, `TELEGRAM_ALLOWED_USERS`, `TELEGRAM_TOPIC_*`). |
| Docs at `{data}/workspace/docs`, never `{data}/docs` | Docs are workspace-adjacent reference material seeded every boot. Migrating agents (Mimir layout) move once in a wrapper entrypoint. |
| `automations.model` is required per project | A baked default would silently drift models between agents sharing one image. No default, no drift. |
| Cron reconcile heals drift | Stored schedule shapes are compared precisely; anything legacy or foreign is treated as drift and healed with exactly one idempotent `cron edit`. A missing or broken spec aborts the whole run rather than pruning live jobs. |
| Automations are image-baked, never host-mounted | Writable cron prompt files would let the agent rewrite its own schedules. |

## Escape hatch: wrapper entrypoints

Projects with one-off needs do not get hooks or plugin loading in the base.
They write a wrapper entrypoint: import the phases, run what you need,
delegate the rest. Phases you can call directly include `load_agent_spec`,
`first_boot_setup`, `reconcile_config`, `reconcile_mcp`,
`reconcile_plugins`, `authenticate_gh`, `seed_content`, `post_startup`, and
`main`. A minimal wrapper that adds a one-time legacy docs move and
otherwise boots standard:

```python
#!/usr/bin/env python3
"""Wrapper entrypoint: one project one-off, then the standard boot."""
import shutil
import sys

sys.path.insert(0, "/opt/agent")
import entrypoint

# One-time legacy migration: {data}/docs -> {data}/workspace/docs.
data = entrypoint.data_dir()
legacy = data / "docs"
if legacy.is_dir():
    target = data / "workspace" / "docs"
    if target.exists():
        shutil.rmtree(target)
    shutil.move(str(legacy), str(target))

raise SystemExit(entrypoint.main(sys.argv[1:]))
```

Point `ENTRYPOINT` at the wrapper (keeping tini in front of it) and it
replaces the stock boot entirely; interleaving custom steps between phases
works the same way, since each phase is a plain function over `(spec, env)`.

## Image versioning

Images publish to `ghcr.io/tankdonut/agent-base` under date tags
(`YYYY.MM.DD`, e.g. `ghcr.io/tankdonut/agent-base:2026.08.21`). There is no
floating `latest`: agent containers are long-running and their boot
behaviour should change only when a human bumps the pin. Pin the exact tag
in every project `FROM` and bump it deliberately; date tags make the diff
reviewable and a bad bump reversible.

## CI pattern

Downstream projects compose CI from `tankdonut/github-actions` rather than
hand-rolling workflows:

- lint/test job: the `pre-commit` action plus `setup-python-uv`, running
  the repo's hooks and `python3 -m unittest discover -s container`;
- a `--validate-spec` step against the project image so a spec regression
  fails the build, not the boot;
- publish job: `build-and-publish-image.yaml` via `workflow_call`, emitting
  the date-tagged image.

This repo runs the same pattern it prescribes. The reusable pieces live in
the actions repo; projects reference them instead of copying YAML.

## Extension checklist

1. Copy `templates/env.example` to `.env` (or `secrets/agent.env`) and keep
   only the variables your spec references; the base vars can stay
   commented defaults.
2. Write `spec.json` starting from `templates/spec.example.json`; consult
   the fixtures for local-MCP versus remote-MCP shapes.
3. Create `workspace/` from `templates/workspace/` (`AGENTS.md`, `SOUL.md`,
   `USER.md`, `MEMORY.md`) and fill in the persona placeholders.
4. Assemble `skills/` and `docs/` as image-baked content; remember skills
   and docs are replaced wholesale every boot.
5. Write `automations/*.md` (name must match the file stem; exactly one of
   `every` or `cron`; `deliver`; optional `topic-env`).
6. Write the thin Dockerfile per Quick start, pinning the current date tag.
7. Add the `agent` service from `templates/compose.agent.yml` and, for
   development, the overlay from `templates/compose.dev.agent.yml`.
8. Stand up CI per the pattern above, including the `--validate-spec` gate.

Validate early: `docker run --rm --env-file .env <image> --validate-spec`
catches schema, templating, and automation errors in seconds.

## Migrations

### Freya (grow-agent)

Landing in a follow-up commit.

### Mimir (trade-agent)

Landing in a follow-up commit.

## Agent contract

Repo conventions for working on this repository (commands, fail-closed
loader rules, secrets handling, `container/` file-mobility warnings, CI
composition) live in [`AGENTS.md`](../AGENTS.md). This document defines the
contract for projects consuming the image; `AGENTS.md` governs development
of the image itself.
