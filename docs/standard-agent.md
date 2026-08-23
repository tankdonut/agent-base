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
FROM ghcr.io/tankdonut/agent-base:2026.08.22

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
does not survive restarts). The base image ships the `gh` binary — Debian
has no gh package, so the Dockerfile installs it from the cli.github.com
apt repo; projects enabling `gh_auth` need no extra install step.
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

One spec-dependent variable is nevertheless load-gated: `ZAI_API_KEY`
must be set whenever `setup.auth_choice` is `zai-coding-*` — the provider's
setup consumes it directly (not via `{env:...}` refs), so the loader checks
its presence itself and aborts with the JSON path (`setup.auth_choice`) and
the variable name before any container work runs.

## spec.json reference

Schema v1. Loading is strict and fail-closed: an unknown key at any nesting
level aborts the boot, there are no warnings and no best-effort parsing,
and every error message starts with the JSON path of the offending node
(`mcp_servers[2].url: ...`). The full key set:

| Section | Keys | Notes |
| --- | --- | --- |
| `specVersion` | (none) | Must be `1`. Anything else is rejected before any other check. |
| `agent` | `name` | Required, non-empty. Reaches logs and seed messages. |
| `setup` | `auth_choice` | Required. Passed to `openclaw setup --auth-choice` on first boot (e.g. `zai-coding-global`). `zai-coding-*` choices require `ZAI_API_KEY` in the environment — the loader fails closed naming the var, and a setup that still fails aborts the boot with a named-var hint (exit 1) instead of crash-looping. |
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
`timeout` (seconds, local and remote) caps the startup probe. `if_env` skips the
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
     mutation — invalid `every:` durations or `cron:` expressions are
     load errors, not silent drift) and drift-healing (see decisions
     below). Every CLI spawn carries a 60s timeout, so a hung cron call
     cannot hang the forked child.
   - Memory reindex (unless `AGENT_MEMORY_REINDEX=0`): clear stale
     reindex locks, check status, then full rebuild, incremental pass, or
     skip. Three attempts with 10s backoff; a degraded success
     (`chunks_vec not updated` on stderr, vectors skipped) counts as
     retryable because it is the memory-search-offline case.
   - Disable skills flagged by `openclaw doctor --lint` as not ready, and
     re-enable skills this reconcile disabled earlier once their finding
     clears (tracked in `{data}/doctor-disabled-skills`; skills the
     operator disabled by other means are never touched).
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
(`YYYY.MM.DD`, e.g. `ghcr.io/tankdonut/agent-base:2026.08.22`). There is no
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

Each guide below is the exact cutover for that project onto
`ghcr.io/tankdonut/agent-base:2026.08.22`. Both keep their existing named
volumes: the base swap changes the image and entrypoint only, never volume
data.

### Freya (grow-agent)

#### 1. What changes

| File | Action | Notes |
| --- | --- | --- |
| `freya/Dockerfile` | replace | Thin FROM + COPYs (below); no ENTRYPOINT, the base carries tini and the entrypoint chain. |
| `freya/spec.json` | create | Declarative boot contract (below). |
| `freya/scripts/automations/` | rename to `freya/automations/` | Base contract location; content unchanged (six jobs, `topic-env` headers stay). |
| `freya/scripts/freya_entrypoint.py` | delete | Boot logic lives in the base entrypoint. |
| `freya/scripts/seed_automations.py` | delete | The base ships `seed_automations.py`. |
| `freya/scripts/test_freya_entrypoint.py`, `freya/scripts/test_seed_automations.py` | delete | The boot pipeline's suite lives in this repo (`container/`). |
| `freya/skills/persist-state/SKILL.md`, `freya/skills/persist-state/scripts/persist-state.sh` | edit | Rename `FREYA_GIT_TOKEN` to `AGENT_GIT_TOKEN` (env read + error text). |
| `freya/.env.example` | edit | Renames per the table below. |
| `compose.yml` | edit | Port interpolation rename only. |
| `compose.dev.yml` | edit | `AGENT_SKIP_SEED=1` plus `/opt/seed/*` mounts. |
| `make.sh` | edit | New `REQUIRED_VARS` (below). |

#### 2. Thin Dockerfile

The base image already has python3 (no pip), gh, tini, the entrypoint
chain, and the `node` user. Freya adds the `ac-infinity-mcp` console
script, the approvals plugin, and its content. Build context stays the
repo root:

```dockerfile
FROM ghcr.io/tankdonut/agent-base:2026.08.22

# hadolint ignore=DL3002
USER root

# ac-infinity-mcp console script on PATH (base ships python3 but no pip).
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3-pip \
    && rm -rf /var/lib/apt/lists/*
COPY tools/ac-infinity-mcp /tmp/ac-infinity-mcp
RUN pip3 install --break-system-packages --no-cache-dir /tmp/ac-infinity-mcp \
    && rm -rf /tmp/ac-infinity-mcp

COPY --chown=node:node freya/spec.json     /opt/agent/spec.json
COPY --chown=node:node freya/automations/  /opt/agent/automations/
COPY --chown=node:node freya/workspace/    /opt/seed/workspace/
COPY --chown=node:node freya/skills/       /opt/seed/skills/
COPY --chown=node:node knowledge/content/  /opt/seed/docs/

# Local plugin seed path: /opt/seed/plugins/<name>. Plugins are not part of
# the base seed table; spec plugins[].source points here (absolute path).
COPY --chown=node:node tools/grow-approval-gate/ /opt/seed/plugins/grow-approval-gate/

USER node
```

#### 3. spec.json

Two schema realities shape this spec. Config `path` strings are never
templated (only values are), so the Telegram group ID is baked literally
into the group paths; keep `TELEGRAM_GROUP_ID` equal to it, the var acts
only as the on/off guard. Template resolution is eager, so every var
referenced by `{env:...}` or `split_csv` must be set (non-empty) at every
boot; `make.sh secrets check` enforces exactly that set:

```json
{
  "specVersion": 1,
  "agent": { "name": "Freya" },
  "setup": { "auth_choice": "zai-coding-global" },
  "model": { "fallback": "zai/glm-4.7" },
  "config": [
    { "path": "channels.telegram.dmPolicy", "value": "allowlist" },
    { "path": "channels.telegram.allowFrom", "value": "{env:TELEGRAM_ALLOWED_USERS}", "split_csv": true, "if_env": ["TELEGRAM_ALLOWED_USERS"] },
    { "path": "agents.defaults.heartbeat.target", "value": "telegram", "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "agents.defaults.heartbeat.to", "value": "{env:TELEGRAM_CHAT_ID}", "strict": true, "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "agents.defaults.heartbeat.directPolicy", "value": "allow", "strict": true, "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "agents.defaults.utilityModel", "value": "zai/glm-4.7" },
    { "path": "tools.web.search.enabled", "value": true },
    { "path": "tools.web.search.provider", "value": "duckduckgo" },
    { "path": "agents.defaults.memorySearch.enabled", "value": true },
    { "path": "agents.defaults.memorySearch.provider", "value": "local" },
    { "path": "agents.defaults.memorySearch.extraPaths", "value": ["{data}/workspace/docs", "{data}/workspace/journal"] },
    { "path": "channels.telegram.capabilities.inlineButtons", "value": "all" },
    { "path": "channels.telegram.mediaMaxMb", "value": 20 },
    { "path": "audit.enabled", "value": true },
    { "path": "channels.telegram.groupPolicy", "value": "allowlist", "if_env": ["TELEGRAM_GROUP_ID"] },
    { "path": "channels.telegram.groups.-1001234567890.requireMention", "value": true, "if_env": ["TELEGRAM_GROUP_ID"] },
    { "path": "channels.telegram.groups.-1001234567890.enabled", "value": true, "if_env": ["TELEGRAM_GROUP_ID"] },
    { "path": "channels.telegram.groups.-1001234567890.allowFrom", "value": "{env:TELEGRAM_GROUP_ALLOWED_USERS}", "split_csv": true, "if_env": ["TELEGRAM_GROUP_ID", "TELEGRAM_GROUP_ALLOWED_USERS"] },
    { "path": "approvals.plugin.enabled", "value": true, "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "approvals.plugin.mode", "value": "targets", "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "approvals.plugin.agentFilter", "value": ["main"], "if_env": ["TELEGRAM_CHAT_ID"] },
    {
      "path": "approvals.plugin.targets",
      "value": [
        {
          "channel": "telegram",
          "to": "{env:TELEGRAM_CHAT_ID}",
          "threadId": "{env:TELEGRAM_TOPIC_APPROVALS}"
        }
      ],
      "if_env": ["TELEGRAM_CHAT_ID", "TELEGRAM_TOPIC_APPROVALS"]
    },
    { "path": "channels.telegram.execApprovals.approvers", "value": "{env:TELEGRAM_APPROVERS}", "split_csv": true, "if_env": ["TELEGRAM_APPROVERS"] },
    { "path": "plugins.allow", "value": ["grow-approval-gate", "llama-cpp"] },
    { "path": "plugins.entries.grow-approval-gate.enabled", "value": true },
    { "path": "plugins.entries.grow-approval-gate.hooks.allowConversationAccess", "value": true },
    { "path": "plugins.entries.grow-approval-gate.config.gatedTools", "value": ["set_port_mode", "set_stage_thresholds", "calibrate_sensor"] },
    { "path": "plugins.entries.grow-approval-gate.config.agentName", "value": "Freya", "strict": true },
    { "path": "secrets.providers.default", "value": { "source": "env" }, "if_env": ["OPENCLAW_GATEWAY_TOKEN"] },
    { "path": "gateway.auth.token", "value": { "source": "env", "provider": "default", "id": "OPENCLAW_GATEWAY_TOKEN" }, "if_env": ["OPENCLAW_GATEWAY_TOKEN"] }
  ],
  "channels": [
    { "type": "telegram" }
  ],
  "mcp_servers": [
    {
      "name": "ac-infinity",
      "command": "ac-infinity-mcp",
      "env": {
        "AC_INFINITY_EMAIL": "{env:AC_INFINITY_EMAIL}",
        "AC_INFINITY_PASSWORD": "{env:AC_INFINITY_PASSWORD}"
      },
      "timeout": 60
    },
    {
      "name": "grow-docs",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "{data}/workspace/docs", "{data}/workspace/journal"]
    }
  ],
  "plugins": [
    { "name": "grow-approval-gate", "source": "/opt/seed/plugins/grow-approval-gate" }
  ],
  "features": { "gh_auth": true },
  "automations": { "model": "zai/glm-4.7" }
}
```

Replace `-1001234567890` with the real supergroup ID. The approvals target
always carries `threadId` (Freya's deployment uses forum topics); deployments
without the approvals topic delete that field and the
`TELEGRAM_TOPIC_APPROVALS` references.

#### 4. Env and secret changes

| Old name | New name | Read by |
| --- | --- | --- |
| `FREYA_SKIP_SEED` | `AGENT_SKIP_SEED` | Base, dev overlay. |
| `FREYA_MANAGE_CONFIG` | `AGENT_MANAGE_CONFIG` | Base, optional. |
| `FREYA_MEMORY_REINDEX` | `AGENT_MEMORY_REINDEX` | Base, optional. |
| `FREYA_GIT_TOKEN` | `AGENT_GIT_TOKEN` | Base `gh auth` (features.gh_auth) and the persist-state skill (edited). |
| `TELEGRAM_HOME_CHANNEL` | `TELEGRAM_CHAT_ID` | Spec heartbeat/approvals templates and cron delivery. |
| `FREYA_GATEWAY_PORT` | `AGENT_GATEWAY_PORT` | Compose port interpolation, host-side only. |

Unchanged: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_ALLOWED_USERS`, `ZAI_API_KEY`,
`OPENCLAW_GATEWAY_TOKEN`, `AC_INFINITY_EMAIL`/`AC_INFINITY_PASSWORD`,
`AUTOMATION_MODEL` (manual-run fallback; the boot always passes
`automations.model`), `FREYA_GIT_NAME`/`FREYA_GIT_EMAIL`/`FREYA_GIT_REMOTE`
(skill-side), and the `TELEGRAM_TOPIC_*` family (automation `topic-env`
headers). New required entries: `TELEGRAM_APPROVERS`,
`TELEGRAM_TOPIC_APPROVALS`, `TELEGRAM_GROUP_ID`,
`TELEGRAM_GROUP_ALLOWED_USERS`. `make.sh` becomes:

```sh
REQUIRED_VARS=(
  OPENCLAW_GATEWAY_TOKEN
  ZAI_API_KEY
  TELEGRAM_BOT_TOKEN
  TELEGRAM_ALLOWED_USERS
  TELEGRAM_CHAT_ID
  TELEGRAM_APPROVERS
  TELEGRAM_GROUP_ID
  TELEGRAM_GROUP_ALLOWED_USERS
  TELEGRAM_TOPIC_APPROVALS
  AC_INFINITY_EMAIL
  AC_INFINITY_PASSWORD
  AGENT_GIT_TOKEN
)
```

#### 5. Compose changes

Only the `ports` line and the dev overlay change in the `freya` service;
`build.dockerfile: freya/Dockerfile`, `env_file: freya/.env`,
`security_opt`, the healthcheck, and the `freya-data` volume (it persists)
all stay:

```yaml
    ports:
      - "127.0.0.1:${AGENT_GATEWAY_PORT:-18789}:18789"
```

`compose.dev.yml` adopts the base dev contract (mounts shadow the seed, not
the live data dir; the old journal bind mount is dropped, journal files stay
volume-side):

```yaml
services:
  freya:
    userns_mode: keep-id
    environment:
      - AGENT_SKIP_SEED=1
    volumes:
      - ./freya/workspace:/opt/seed/workspace:z
      - ./freya/skills:/opt/seed/skills:z
      - ./knowledge/content:/opt/seed/docs:z
    restart: ""
```

#### 6. One-time manual ops

The base drops Freya's legacy state migrations (tent-state renames,
strain-to-crop, legacy layout moves). The old image ran them every boot, so
an up-to-date volume already satisfies them; run the equivalent by hand once
(ordered before the first new-image boot), or accept the seed defaults on
volumes restored from old backups:

```sh
podman compose -f compose.yml stop freya
podman run --rm -i -v freya-data:/data docker.io/library/python:3.12-slim python3 - <<'EOF'
import json
import shutil
from pathlib import Path

data = Path("/data")
journal = data / "workspace" / "journal"

# old migrate_state_file: current-state.json -> tent-state.json
old_state, new_state = journal / "current-state.json", journal / "tent-state.json"
if old_state.exists() and not new_state.exists():
    old_state.rename(new_state)

# old migrate_strain_to_crop: active_run.strain key becomes active_run.crop
state = journal / "tent-state.json"
if state.is_file():
    doc = json.loads(state.read_text(encoding="utf-8"))
    active_run = doc.get("active_run")
    if isinstance(active_run, dict) and "strain" in active_run and "crop" not in active_run:
        doc["active_run"] = {
            ("crop" if key == "strain" else key): value for key, value in active_run.items()
        }
        state.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")

# old migrate_legacy_layout: {data}/journal/* -> workspace/journal/, drop {data}/docs
legacy_journal = data / "journal"
if legacy_journal.is_dir():
    journal.mkdir(parents=True, exist_ok=True)
    for entry in legacy_journal.iterdir():
        if not (journal / entry.name).exists():
            shutil.move(str(entry), str(journal / entry.name))
    if not any(legacy_journal.iterdir()):
        legacy_journal.rmdir()
if (data / "docs").is_dir():
    shutil.rmtree(data / "docs")

print("freya state migration complete")
EOF
```

Two follow-ups. A warm volume carries a stale `grow-docs` MCP entry with
pre-workspace paths; the new reconcile sees the name and skips re-registering,
so unset it once and the first standard boot re-adds it with `{data}` paths:

```sh
podman run --rm -v freya-data:/home/node/.openclaw \
  --entrypoint openclaw ghcr.io/tankdonut/agent-base:2026.08.22 \
  mcp unset grow-docs
```

Pre-JSON volumes (no `tent-state.json` at all): copy
`freya/workspace/journal/tent-state.json` from the repo into the volume's
`workspace/journal/` and set `active_run.stage` by hand from the
`**Current Stage:**` line of `docs/journal/tent-state.md` before the first
boot; the docs reseed overwrites that markdown file, so it is the only
record. Then run the first validation locally (the same command CI uses):

```sh
podman run --rm \
  -v ./freya/spec.json:/opt/agent/spec.json:ro \
  -v ./freya/automations:/opt/agent/automations:ro \
  --env-file freya/.env \
  --entrypoint python3 \
  ghcr.io/tankdonut/agent-base:2026.08.22 \
  /opt/agent/entrypoint.py --validate-spec
```

#### 7. Test and CI changes

The ported suites are deleted with the scripts they tested (table in step
1); grow-agent keeps no agent-boot tests, this repo's `container/` suite is
the pipeline's coverage. CI (and optionally a local make target) replaces
them with the `--validate-spec` run above. In CI, pass an env file with
dummy non-empty values for every `split_csv` and `{env:...}` var: the run
only parses and resolves, it applies nothing and logs no values. The
existing hadolint hook keeps linting the thin Dockerfile; there was no
python hook to remove.

#### 8. Rollback

The pre-migration commit still builds the old bespoke image (old Dockerfile
plus `freya/scripts/`), and `freya-data` is untouched by the swap: rolling
back is a checkout, a rebuild, and restoring the old env names
(`TELEGRAM_HOME_CHANNEL`, `FREYA_*`) in `freya/.env`. State written by the
standard image is layout-compatible with the old entrypoint.

### Mimir (trade-agent)

#### 1. What changes

| File | Action | Notes |
| --- | --- | --- |
| `agent/Dockerfile` | replace | Thin FROM + COPYs (below). |
| `agent/spec.json` | create | Declarative boot contract (below). |
| `agent/scripts/automations/` | rename to `agent/automations/` | Four jobs, unchanged. |
| `agent/scripts/mimir_entrypoint.py` | delete | Boot logic lives in the base entrypoint. |
| `agent/scripts/seed_automations.py` | delete | The base ships `seed_automations.py`. |
| `agent/scripts/test_mimir_entrypoint.py`, `agent/scripts/test_seed_automations.py` | delete | Suites live in this repo. |
| `agent/.env.example` | edit | `AGENT_*` optional entries, load-time-required var notes. |
| `compose.yml` | no change | Same build path, volume, env wiring. |
| `compose.dev.yml` | edit | `MIMIR_SKIP_SEED` rename. |
| `make.sh` | edit | `secrets_check` additions (below); `write_agent_env` vars unchanged. |
| `.pre-commit-config.yaml` | edit | Drop the `python-test` hook. |

#### 2. Thin Dockerfile

The base carries python3, tini, the entrypoint chain, and the `node` user;
Mimir adds only content. Build context stays the repo root:

```dockerfile
FROM ghcr.io/tankdonut/agent-base:2026.08.22

COPY --chown=node:node agent/spec.json     /opt/agent/spec.json
COPY --chown=node:node agent/automations/  /opt/agent/automations/
COPY --chown=node:node agent/workspace/    /opt/seed/workspace/
COPY --chown=node:node .opencode/skills/   /opt/seed/skills/
COPY --chown=node:node knowledge/content/  /opt/seed/docs/
```

#### 3. spec.json

```json
{
  "specVersion": 1,
  "agent": { "name": "Mimir" },
  "setup": { "auth_choice": "zai-coding-global" },
  "model": { "fallback": "zai/glm-4.7" },
  "config": [
    { "path": "channels.telegram.dmPolicy", "value": "allowlist" },
    { "path": "channels.telegram.allowFrom", "value": "{env:TELEGRAM_ALLOWED_USERS}", "split_csv": true, "if_env": ["TELEGRAM_ALLOWED_USERS"] },
    { "path": "agents.defaults.heartbeat.target", "value": "telegram", "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "agents.defaults.heartbeat.to", "value": "{env:TELEGRAM_CHAT_ID}", "strict": true, "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "agents.defaults.heartbeat.directPolicy", "value": "allow", "strict": true, "if_env": ["TELEGRAM_CHAT_ID"] },
    { "path": "agents.defaults.utilityModel", "value": "zai/glm-4.7" },
    { "path": "tools.web.search.enabled", "value": true },
    { "path": "tools.web.search.provider", "value": "duckduckgo" },
    { "path": "agents.defaults.memorySearch.enabled", "value": true },
    { "path": "agents.defaults.memorySearch.provider", "value": "local" },
    { "path": "agents.defaults.memorySearch.extraPaths", "value": ["{data}/workspace/docs", "{data}/workspace/journal"] },
    { "path": "secrets.providers.default", "value": { "source": "env" }, "if_env": ["OPENCLAW_GATEWAY_TOKEN"] },
    { "path": "gateway.auth.token", "value": { "source": "env", "provider": "default", "id": "OPENCLAW_GATEWAY_TOKEN" }, "if_env": ["OPENCLAW_GATEWAY_TOKEN"] }
  ],
  "channels": [
    { "type": "telegram" }
  ],
  "mcp_servers": [
    { "name": "trade-agent", "url": "http://mcp:9090" },
    { "name": "defillama", "url": "https://mcp.defillama.com/mcp" },
    { "name": "tradingview", "command": "npx", "args": ["-y", "tradingview-mcp-server"] },
    { "name": "alpha-vantage", "url": "https://mcp.alphavantage.co/mcp?apikey={env:ALPHAVANTAGE_API_KEY}" },
    {
      "name": "lunarcrush",
      "url": "https://lunarcrush.ai/mcp",
      "headers": { "Authorization": "Bearer {env:LUNARCRUSH_API_KEY}" }
    },
    {
      "name": "postgres",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-postgres", "{env:DATABASE_URL}"]
    }
  ],
  "plugins": [],
  "features": { "gh_auth": false },
  "automations": { "model": "zai/glm-5.2" }
}
```

Two differences from the old script are intentional. `memorySearch.extraPaths`
is new: the old entrypoint never set it, and the docs standard
(`{data}/workspace/docs`, there is no other destination) needs both paths on
the index. `automations.model` is `zai/glm-5.2` per project decision.

#### 4. Env and secret changes

| Old name | New name | Where |
| --- | --- | --- |
| `MIMIR_SKIP_SEED` | `AGENT_SKIP_SEED` | `compose.dev.yml`. |
| `MIMIR_MANAGE_CONFIG` | `AGENT_MANAGE_CONFIG` | Optional, `secrets/agent.env`. |
| `MIMIR_MEMORY_REINDEX` | `AGENT_MEMORY_REINDEX` | Optional, `secrets/agent.env`. |

`TELEGRAM_CHAT_ID` (already the base standard), `AUTOMATION_MODEL`
(manual-run fallback only), and `write_agent_env`'s variable list are
unchanged. Eager template resolution makes these load-time-required and
non-empty at every boot: `TELEGRAM_ALLOWED_USERS` (`split_csv` fails closed
on empty), `TELEGRAM_CHAT_ID`, `ALPHAVANTAGE_API_KEY`, `LUNARCRUSH_API_KEY`,
`DATABASE_URL` (compose sets the last via `x-db-env`). Add the first four to
`make.sh secrets_check`'s `agent.env` requires alongside
`OPENCLAW_GATEWAY_TOKEN`, `ZAI_API_KEY`, and `DATABASE_URL`.

#### 5. Compose changes

`compose.yml` needs no edits: `build.dockerfile: agent/Dockerfile` now
points at the thin file, `env_file: ./secrets/agent.env`, the
`trade-agent_agent-data` volume (pinned compose project name), `x-db-env`,
the `127.0.0.1:18789` publish, the healthcheck, and `depends_on: mcp` all
persist. The dev overlay renames one variable; its mounts already target
`/opt/seed/*`:

```yaml
    environment:
      - AGENT_SKIP_SEED=1
```

#### 6. One-time manual ops

Move the volume's legacy docs layout once, ordered before the first
new-image boot (afterwards the image re-seeds `workspace/docs` every boot;
this move clears `{data}/docs` and preserves any file that drifted from the
image):

```sh
podman compose -f compose.yml stop agent
podman run --rm -v trade-agent_agent-data:/data docker.io/library/busybox:latest sh -c \
  'mkdir -p /data/workspace/docs && cp -a /data/docs/. /data/workspace/docs/ && rm -rf /data/docs'
```

No other state moves: skills already seed to the same path, workspace is
first-boot-only and present, and the base creates `workspace/journal` every
boot. The `memorySearch.extraPaths` addition is part of the spec above, not
an operation.

#### 7. Test and CI changes

Delete `agent/scripts/test_mimir_entrypoint.py` and
`agent/scripts/test_seed_automations.py` with the scripts, and remove the
pre-commit hook that ran them:

```yaml
      - id: python-test
        name: python test (agent scripts)
        entry: python3 -m unittest discover -s agent/scripts -p "test_*.py"
        language: system
        files: ^agent/scripts/
        pass_filenames: false
```

Replace with the `--validate-spec` gate (CI dummy env values are fine; the
run parses and resolves only, applying and logging nothing):

```sh
podman run --rm \
  -v ./agent/spec.json:/opt/agent/spec.json:ro \
  -v ./agent/automations:/opt/agent/automations:ro \
  --env-file secrets/agent.env \
  --entrypoint python3 \
  ghcr.io/tankdonut/agent-base:2026.08.22 \
  /opt/agent/entrypoint.py --validate-spec
```

Slot the step into the existing lint/test job (composed from
`tankdonut/github-actions`), after the project image builds.

#### 8. Rollback

The pre-migration commit still builds the old image (`agent/Dockerfile` plus
`agent/scripts/`), and `agent-data` is untouched by the swap. A rollback
boot re-creates `{data}/docs` from its own image content; the copy at
`workspace/docs` is a harmless leftover it ignores.

## Agent contract

Repo conventions for working on this repository (commands, fail-closed
loader rules, secrets handling, `container/` file-mobility warnings, CI
composition) live in [`AGENTS.md`](../AGENTS.md). This document defines the
contract for projects consuming the image; `AGENTS.md` governs development
of the image itself.
