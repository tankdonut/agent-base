# Standard Agent Contract

`agent-base` is the shared OpenClaw agent image for tankdonut projects. One
image carries the whole boot pipeline: spec validation, first-boot setup,
config/MCP/plugin reconciliation, content seeding, cron scheduling, and
memory indexing. A project contributes no boot code of its own. It ships a
thin Dockerfile that drops content into the image (a `spec.json`, three seed
directories, and an automations directory) and inherits everything else.

The image distills two agents that already ran this pipeline as bespoke
scripts, grow-agent and trade-agent; their names survive in the test
fixtures. Their legacy per-project environment names are gone. One
`AGENT_*` vocabulary covers both, declared per project in `spec.json`.

This document is the contract. The code in `container/` is its
implementation: `spec.py` (loader), `entrypoint.py` (boot phases),
`seed_automations.py` (cron reconciler). Worked specs live in
`examples/spec.example.json` (golden example) and
`tests/fixtures/*grow-agent-like/` plus `tests/fixtures/*trade-agent-like/`
(real boot-tested fixtures; consult them rather than copying them whole).
Repo conventions for developing the image itself live in
[`AGENTS.md`](../AGENTS.md). Everything outside the image (host prep, TLS
proxy, updates, platforms) lives in [`docs/deployment.md`](deployment.md).

## Contents

- [Quick start](#quick-start)
- [Environment contract](#environment-contract)
- [Model providers via LiteLLM](#model-providers-via-litellm)
- [Upgrades](#upgrades)
- [spec.json reference](#specjson-reference)
- [Seeded content and lifecycle](#seeded-content-and-lifecycle)
- [Boot sequence](#boot-sequence)
- [Cron trigger scripts](#cron-trigger-scripts)
- [Graceful shutdown](#graceful-shutdown)
- [Standardization decisions](#standardization-decisions)
- [Runaway and budget controls](#runaway-and-budget-controls)
- [Python tools (optional)](#python-tools-optional)
- [Escape hatch: wrapper entrypoints](#escape-hatch-wrapper-entrypoints)
- [Image versioning](#image-versioning)
- [Supply chain](#supply-chain)
- [CI pattern](#ci-pattern)
- [Extension checklist](#extension-checklist)

## Quick start

A project image is a `FROM` line, five required `COPY` lines, and an
optional sixth for trigger scripts. The base image
carries the entrypoint chain (tini → `entrypoint.py`, which spawns and
supervises the container CMD, `openclaw gateway`; see
[Graceful shutdown](#graceful-shutdown)) and the Python runtime. Your
Dockerfile inherits all of it and adds only content:

```dockerfile
FROM ghcr.io/tankdonut/agent-base:2026.08.24.1

# The declarative boot contract (container/spec.py loads it fail-closed).
COPY --chown=node:node spec.json /opt/agent/spec.json

# Seed content: workspace first boot, skills + docs every boot.
COPY --chown=node:node workspace/ /opt/seed/workspace/
COPY --chown=node:node skills/ /opt/seed/skills/
COPY --chown=node:node docs/ /opt/seed/docs/

# Cron specs: image-baked, never host-mounted.
COPY --chown=node:node automations/ /opt/agent/automations/

# Trigger scripts (optional): read-only like automations.
COPY --chown=node:node scripts/ /opt/agent/scripts/
```

Validate the spec and automations in CI without touching a volume:

```sh
docker run --rm --env-file .env <image> --validate-spec
```

Exit 0 means both parsed; exit 1 prints the reason on stderr. The gateway
never starts in this mode and nothing is mutated.

To use the GitHub CLI from agent sessions, enable it in the spec and pass
the token. A classic PAT with `repo` scope is the reliable choice:

```json
"features": { "gh_auth": true }
```

```text
AGENT_GIT_TOKEN=ghp_...
```

When `features.gh_auth` is true, the entrypoint runs `gh auth login
--with-token` on every boot. gh state lives outside the data volume, so it
does not survive restarts. The base image ships the `gh` binary: Debian has
no gh package, so the Dockerfile installs it from the cli.github.com apt
repo, and projects enabling `gh_auth` need no extra install step. Failure
is non-fatal and the token is never logged.

Wire the runtime up with `examples/compose.agent.yml` (service snippet),
then fill `.env` from `examples/env.example`.

## Environment contract

The base image reads only the variables below. Everything else in a
project's `.env` is spec-dependent: it reaches the runtime solely through
`spec.json` `{env:NAME}` refs, `if_env` guards, and `split_csv` values, so
its names are the project's choice. `examples/env.example` states the same
split with copy-paste entries.

| Variable | Default | Effect |
| --- | --- | --- |
| `AGENT_SPEC_PATH` | `/opt/agent/spec.json` | Override the spec location (tests, fixtures, wrapper entrypoints). |
| `AGENT_MANAGE_CONFIG` | `1` | `0` skips config/MCP/plugin reconciliation, for operators who manage `openclaw.json` manually. |
| `AGENT_SKIP_SEED` | `0` | `1` skips content seeding only; reconciliation still runs. (The former dev-overlay consumer is retired: seeding is image-baked.) |
| `AGENT_SYNC` | `0` | `1` for one boot forces a workspace re-seed on an existing volume: seeded persona files are overwritten with image content, while agent-written files outside the seed set (journal, memories) survive. The named alternative to deleting the volume. |
| `AGENT_MEMORY_REINDEX` | `1` | `0` skips the post-startup memory reindex. |
| `AGENT_GIT_TOKEN` | unset | gh token, used only when `features.gh_auth` is true. Never logged. |
| `AGENT_AUTOMATIONS_DIR` | `/opt/agent/automations` | Override the automations directory. |
| `AGENT_SCRIPTS_DIR` | `/opt/agent/scripts` | Override the trigger-scripts directory resolved for `trigger-script:` automation headers (sibling of the automations directory). |
| `AGENT_AUTOMATION_TRIGGERS` | unset | `1` opts the deployment into cron trigger scripts: the cron seed arms the Gateway's `cron.triggers.enabled` config and `trigger-script:` automations seed normally. Without the opt-in, a trigger automation aborts cron seeding loudly. See [Cron trigger scripts](#cron-trigger-scripts). |
| `AUTOMATION_MODEL` | unset | Cron model fallback when `--model` is not passed. The entrypoint always passes `spec.automations.model`, so this matters only for manual `seed_automations.py` runs. A job's `model:` header overrides it for that job. |
| `TELEGRAM_CHAT_ID` | unset | Chat ID for cron delivery (base-standardized). Unset means jobs run without Telegram delivery. |
| `AGENT_BASE_VERSION` | baked `ENV` | Image version, from the build ARG; also the OCI label. Read-only signal for the upgrade-backup phase: a delta against `{data}/last-image-version` triggers a verified backup before migration. |
| `AGENT_BACKUP_DIR` | `/backups` | Destination for the upgrade backup (the CLI refuses output inside `{data}`). Mount a named volume at `/backups` to keep archives across containers. |
| `AGENT_SHUTDOWN_GRACE` | `600` | Seconds a shutdown signal (SIGTERM/SIGINT) waits for the gateway and its in-flight automations before their process group is force-killed. `0` forwards the signal, then force-kills at once; invalid values warn and fall back. Keep the engine stop timeout above it. See [Graceful shutdown](#graceful-shutdown). |

The base also bakes two OpenClaw runtime vars into the image (`ENV`):

- `OPENCLAW_SERVICE_REPAIR_POLICY=external`: doctor never attempts service
  lifecycle operations, because the container supervisor owns them.
- `OPENCLAW_NO_AUTO_UPDATE=1`: `openclaw update` refuses. A self-update
  would write to the ephemeral layer and vanish on redeploy; upgrades are
  image bumps.

`OPENCLAW_SUPERVISOR_MODE=external` is a stricter variant that fuses the
restart handoff to the supervisor. The base does not set it; its contract
needs design before adoption.

Two spec-dependent variables are load-gated: `ZAI_API_KEY` whenever
`setup.auth_choice` is `zai-coding-*`, and `LITELLM_API_KEY` whenever it is
`litellm-api-key`. The providers' setup consumes them directly rather than
through `{env:...}` refs, so the loader checks presence itself and aborts
with the JSON path (`setup.auth_choice`) and the variable name before any
container work runs. The gate checks presence, not validity: a near-miss
token passes the loader and fails in setup, which aborts the boot with a
named-var hint (exit 1) instead of crash-looping.

Do not set `OPENCLAW_HOME`. The entrypoint pops it at import: OpenClaw
treats it as a home directory and appends `.openclaw/` inside it, which
double-nests the data dir. The default (`~/.openclaw`, i.e.
`/home/node/.openclaw` in the container) is correct.

Resolved secret values never reach logs. Reconciliation warnings name the
config key that failed, never the value being set; loader warnings name
env vars, never values. Canary tests lock this.

## Model providers via LiteLLM

`setup.auth_choice: "litellm-api-key"` points the agent at a LiteLLM proxy
as its only model-provider surface. The deployed shape is a sidecar on its
own compose network (`model-net`: exactly the agent and the proxy).
`agent-net` keeps every other sibling off the gateway L2. The gateway
remains token-gated on every interface, which is what makes pairing the
two acceptable.

- **The proxy is the home of all provider configuration.** Upstream models
  and routing live in `litellm/config.yaml` (`model_list` entries; golden:
  `examples/litellm/config.example.yaml`). Provider API keys live only in
  the litellm service env (`litellm/.env`), referenced from the yaml as
  `os.environ/NAME` and never as literals.
- **The agent holds one key.** `LITELLM_API_KEY` in the agent env is the
  proxy's master key. Mirror it from `LITELLM_MASTER_KEY`: generate both
  env files from one value and keep them equal, because a half-rotation
  fails the proxy healthcheck. OpenClaw's setup consumes the var natively
  and stores it in its agent sqlite store inside `{data}`. The loader gates
  the boot on it exactly like `ZAI_API_KEY` for zai choices.
- **Models are proxy aliases.** `model.fallback`, `automations.model`, and
  per-job `model:` headers reference the proxy's `model_list` aliases.
  Prefer the actual model name as the alias (the convention is
  `litellm/glm-5.3-flash`) so model identity stays legible end to end. A
  distinct alias is a deliberate indirection: load-balance pools, or
  re-pointing a route without a spec edit.
- **baseUrl is seeded, not trusted to defaults.** OpenClaw's own litellm
  provider defaults its base URL to loopback (`http://localhost:4000`),
  unreachable when the proxy is a compose sibling. On every reconcile with
  the `litellm-api-key` auth choice, the base seeds
  `models.providers.litellm.baseUrl` to `http://litellm:4000` (the blessed
  sidecar DNS name) when no spec config entry owns the path. This is the
  same ownership rule as the `gateway.bind=lan` seed. Deployments pointing
  at an external proxy own the path instead:

  ```json
  {"path": "models.providers.litellm.baseUrl", "value": "https://llm.example.com"}
  ```

The proxy runs db-less in this shape (no Postgres).
`LITELLM_MASTER_KEY` is the whole auth surface: no virtual keys, budgets,
or teams, which is the right trade for a single-agent deployment. The
compose `healthcheck` probes `/health/liveliness` (the image ships no
curl; its python3 serves the probe). The service publishes no ports
(agent-reachable only), and the image is digest-pinned from day one
(`ghcr.io/berriai/litellm:v1.100.0@sha256:…`); Renovate bumps the
tag+digest pair together in this repo's prod example, and downstream
projects pin the same way.

`examples/compose.prod.agent.yml` is this shape: the compose file carries
the sidecar on `model-net`, and the two env files (agent + litellm) hold
the key pair. The pinned OpenClaw CLI enables its `litellm` plugin and
writes the provider + auth profile itself during first-boot setup,
including a default `litellm/claude-opus-4-6` model entry that your
`model_list` aliases supersede.

### Migrating an existing agent to LiteLLM

Auth changes do **not** re-run setup on a warm volume: `first_boot_setup`
is gated on `{data}/openclaw.json` being absent, so flipping
`setup.auth_choice` alone leaves OpenClaw without the litellm provider,
plugin entry, and auth profile. The baseUrl seed alone is not enough. The
blessed path is a fresh volume:

1. Export what matters from the old stack (`openclaw backup create`
   inside the running agent container; persona content re-seeds from the
   image anyway).
2. Adopt the litellm shape: the `litellm/` tree, the compose service and
   `model-net`, spec `auth_choice: "litellm-api-key"`, and model refs
   `litellm/<alias>`.
3. Run `docker/podman compose down --volumes`, re-create both env files
   with the matching key pair, then `up`.

Verify the fresh boot took: gateway healthy, `openclaw models list` shows
the `litellm/<alias>` entries, and the litellm service healthcheck passes.
Spec-side errors surface before any boot with the same `--validate-spec`
pass CI runs: `docker run --rm --env-file .env <image> --validate-spec`.

Running the same setup command in-place inside the running container is
the escape hatch (`openclaw setup --non-interactive --auth-choice
litellm-api-key …` with `LITELLM_API_KEY` exported), but it sits outside
the base's gates and is not covered by the test surface. Verify with
`openclaw models list` before retiring the old provider key.

## Upgrades

Bumping a consumer's base image tag is the upgrade path. The base makes it
safe in two ways:

1. **Migrate + verified backup before state changes.** On every boot the entrypoint
   compares `AGENT_BASE_VERSION` (baked `ENV`) against
   `{data}/last-image-version`. When it changes on a warm volume,
   including a volume's first boot on a versioned image, the boot runs
   three steps before any other phase mutates state: a raw byte-level
   snapshot of the state database into the backups directory (best-effort
   restore point taken before any migration touches it), `openclaw
   doctor --fix --non-interactive` (the pinned CLI, 2026.10+, runs its
   state-db schema migrations — e.g. `audit-events-v2` — and refuses both
   backups and gateway startup until they are applied), and `openclaw
   backup create --verify --output /backups` (the CLI refuses
   output inside `{data}`; `AGENT_BACKUP_DIR` relocates it). A failed
   migration or backup **aborts the boot** (exit
   1, named reason): data safety outranks gateway availability during a
   migration. The un-updated marker makes the next boot retry. Backups
   are only as durable as their destination, so mount a named volume at
   `/backups` so archives survive container replacement. Outside
   migrations, the same primitive is available on demand:
   `docker/podman compose exec agent openclaw backup create --verify
   --output /backups`. Copy the archive to host storage for safekeeping.
2. **Opt-in MCP removal + orphan report.** Servers the base registered are
   tracked in `{data}/agent-managed-mcp`. With `features.mcp_prune: true`,
   a server that leaves the spec is unset (`openclaw mcp unset`) with a
   log line. Servers the operator registered by hand are never touched,
   and a spec server merely skipped by `if_env` this boot is still
   spec'd, never removed. `mcp_prune` is default-off because the marker
   lives in agent-writable `{data}`. Without it, drift surfaces as a
   per-boot warning (`MCP server '<name>' registered but not in spec`)
   for anything registered that no spec entry accounts for.

### Release provenance

Released images carry SLSA build provenance (attested into the registry)
and an SPDX SBOM as a release asset; the release notes pin the exact
digest. Pull by digest to make upgrades byte-reproducible:

```text
ghcr.io/tankdonut/agent-base:<tag>@<digest>   # from the release notes
gh attestation verify oci://ghcr.io/tankdonut/agent-base:<tag>@<digest> -R tankdonut/agent-base
```

### Upgrade runbook

By hand:

1. `docker/podman compose down` (stop the agent; a live SQLite volume must
   not be raw-copied).
2. Optional but recommended: copy the newest `/backups/` archive to host
   storage.
3. Bump the base tag in the project Dockerfile
   (`FROM ghcr.io/tankdonut/agent-base:<new-date-tag>`), rebuild, `up`.
4. Expect one log line, `Image changed (<old> → <new>) — creating
   verified backup`, then the normal boot sequence. A de-specified MCP
   server logs `Removed MCP server '<name>' (no longer in spec)`.
5. Verify: gateway healthy, `openclaw mcp list --json` shows exactly
   the spec'd servers, `openclaw cron list --json` shows the seeded jobs, and
   the boot summary (`{data}/status.json`) reports the image version,
   warning count, and completion time.
6. Roll back if anything is wrong: `down`, restore the backup archive per
   `openclaw backup` docs, revert the tag, `up`. The version marker rides
   the volume, so a rollback re-runs its own backup first.

## spec.json reference

Schema v1. Loading is strict and fail-closed: an unknown key at any nesting
level aborts the boot, there are no warnings and no best-effort parsing,
and every error message starts with the JSON path of the offending node
(`mcp_servers[2].url: ...`). The full key set:

| Section | Keys | Notes |
| --- | --- | --- |
| `specVersion` | (none) | Must be `1`. Anything else is rejected before any other check. |
| `agent` | `name` | Required, non-empty. Reaches logs and seed messages. |
| `setup` | `auth_choice` | Required. Passed to `openclaw setup --auth-choice` on first boot (e.g. `litellm-api-key`, `zai-coding-global`). `zai-coding-*` choices require `ZAI_API_KEY`; `litellm-api-key` requires `LITELLM_API_KEY` (see [Environment contract](#environment-contract)). |
| `model` | `fallback`, `thinking` | `fallback` required; registered via `openclaw models fallbacks add` on first boot. `thinking` optional: the reasoning effort, seeded to `agents.defaults.thinkingDefault` (OpenClaw's global thinking default, resolved per turn) on every boot unless a config entry owns the path. Allowed values: `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `adaptive`, `max`; anything else is a load error. Providers map the level onto their own request field (e.g. the Z.AI provider sends `reasoning_effort`; for GLM-5.x an explicit `off` still maps to that API's lowest effort). The base merges `fallback` into an existing `agents.defaults.models` allowlist (see [Boot sequence](#boot-sequence)). |
| `automations` | `model`, `default_tools` | `model` required: the model for cron agent turns. No default exists by design (see [Standardization decisions](#standardization-decisions)). `default_tools` optional: the default tool allow-list for seeded jobs without their own `tools:` header; `["*"]` restores unrestricted. |
| `config` | `path`, `value`, `strict`, `if_env`, `split_csv` | `path` and `value` required; the rest are optional booleans / string lists. Applied in spec order. `path` accepts `{env:...}` tokens (chat IDs stop being baked into git); resolution mirrors `value`, including guard deferral. An item may instead be exactly `{"include": "<preset>"}.` See [config entries](#config-entries). |
| `presets` | name → list of config entries | Named groups spliced into `config` in place via `{"include": ...}`. Names are `lowercase-identifier-ish`. Fail-closed: unknown names, nesting (include inside a preset), and malformed shapes are load errors. Spliced entries behave identically to inline ones (templating, guard deferral, split_csv). |
| `channels` | `type`, `use_env` | `type` required. `use_env` (default `true`) feeds the channel credentials from the environment; the pinned CLI (2026.10+) validates them at add time, so a missing credential aborts first boot with the env var named by the CLI. |
| `mcp_servers` | `name`, `command` or `url`, `args`, `env`, `headers`, `no_probe`, `timeout`, `if_env`, `config`, `auth`, `oauth`, `transport` | Exactly one of `command` (local stdio) and `url` (remote HTTP); specifying both or neither is an error. See [mcp_servers entries](#mcp_servers-entries). |
| `plugins` | `name`, `source` | `source` absent means install `name` from the registry; present means a local plugin directory and must be an absolute path. On reconcile the base seeds `plugins.allow` (its first-boot plugin snapshot ∪ the spec's plugin names, e.g. `["llama-cpp"]` for a plugin-less spec) when neither an existing config value nor an env-active spec entry owns the path, so the gateway's empty-allowlist warning ("discovered non-bundled plugins may auto-load") never fires for an unconfigured consumer. An operator-provided list is never clobbered. |
| `features` | `gh_auth`, `gateway_auth`, `plugin_prune`, `mcp_prune` | All default `false`. See [features](#features). |

### Templating

String values at any depth resolve two tokens at load time, in a single
pass (replacement text is never re-scanned):

- `{env:NAME}`: substituted from the boot environment. A missing `NAME` is
  a load error naming the variable.
- `{data}`: the live data root, `~/.openclaw` (`/home/node/.openclaw`).

Any other brace content, or an unclosed brace, is an error. Template
resolution happens once, eagerly, against the environment the loader was
given; nothing re-resolves later.

`if_env`-guarded entries are the exception, and they enable the
optional-secret pattern. When an entry's guard is unsatisfied at load, its
`value` (and `path`) resolution is deferred (raw tokens preserved) and the
entry is inert; the reconciler skips it. A spec that templates
`{env:TELEGRAM_CHAT_ID}` inside an `if_env: ["TELEGRAM_CHAT_ID"]` entry
boots fine without the variable: optional secrets are genuinely optional.
The fail-closed rule is unchanged for everything else. Unguarded refs, and
refs inside entries whose guard is satisfied, abort the load naming the
variable. The same deferral applies to guarded `mcp_servers` entries:
headers and URLs stay raw when the guard is unsatisfied, and the server is
skipped at reconcile.

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
  present. An unsatisfied guard is a logged skip, never an error. Guards
  must pair the knobs they make optional: `channels.telegram.dmPolicy`
  takes `if_env: [TELEGRAM_ALLOWED_USERS]` alongside
  `channels.telegram.allowFrom`, so an unset variable leaves OpenClaw's
  default DM policy in place instead of an allowlist with an empty list
  (which drops every DM). A spec that instead requires the variable
  (unguarded `allowFrom`) fails closed at load. Both shapes are safe; the
  broken one is the mix.

On warm volumes the entrypoint compares each desired value against
`openclaw.json` directly and shells out only for keys that actually drift,
so a rebooting container costs one JSON read, not N CLI spawns.

### mcp_servers entries

Local (stdio) servers run a command; remote (HTTP) servers point at a URL.

Remote entries default to the CLI's SSE transport. Endpoints that only
speak streamable HTTP (POST-only MCP; LunarCrush and Alpha Vantage are the
known cases) reject the SSE GET handshake, and `bundle-mcp: SSE error:
Non-200 status code (405/400)` at boot is the signature. Pin such entries
with `"transport": "streamable-http"`. The value is validated fail-closed
against the pinned CLI's `--transport` choices (`sse` |
`streamable-http`), applies to remote (`url`) entries only, and
participates in the args-digest drift marker like every other flag:
changing it re-registers the server on the next boot.

Both kinds reconcile idempotently. The entrypoint lists registered
servers, adds the missing, and re-registers an existing one whose resolved
flags (URL, headers, timeout; `{env:...}` rotation included) drifted from
the last boot's. Drift is detected by digesting the resolved add flags and
comparing against the `{data}/agent-mcp-args` marker, because the listing
CLI carries no per-server fields to diff. A hand-registered or pre-marker
server therefore converges to the spec on its next boot; only the one-way
digest is persisted, never the resolved values. A failed re-add leaves
the server absent and the digest stale, so the next boot retries.

`config` is the per-server escape hatch: an object of arbitrary knobs
(`requestTimeoutMs`, `toolFilter`, …) applied every boot as
`mcp.servers.<name>.<key>` via `config set --strict-json`. Values are
templated and guard deferral applies. Key validity is the operator's
responsibility: the runtime does not reject unknown `mcp.servers.*` keys
at the pinned tag, so a typo'd knob lands silently inert.

`no_probe` defaults to `true` (skip the startup probe); set `false` when
you want registration to verify connectivity. `timeout` (seconds, local
and remote) caps the startup probe. `if_env` skips the server when a
listed variable is absent, which is the standard way to make an optional
API-keyed server conditional.

Removing an entry from the spec removes its registration on the next boot
under `features.mcp_prune`, but only for servers the base itself
registered (tracked in `{data}/agent-managed-mcp`). Operator-added servers
are never pruned, `if_env`-skipped entries count as still spec'd, and
anything registered that no spec entry accounts for warns once per boot.

`command` values resolve `{data}` templates, but keep executable surfaces
out of `{data}` (plugin sources and MCP commands). That tree is
agent-writable, and the loader cannot know your mount layout.

#### OAuth remote servers

Remote entries can arm OpenClaw's MCP OAuth flow with first-class keys:

```json
{
  "name": "docs",
  "url": "https://mcp.example.com/mcp",
  "auth": "oauth",
  "oauth": { "identity": "shared", "scope": "docs.read" }
}
```

`auth` must be `"oauth"` (the only documented mode). The `oauth` object
accepts exactly the sub-keys OpenClaw documents, carried verbatim:
`identity` (`"shared"` | `"per-requester"`), `scope`, `authProfileId`.
Values are never `{env:}`-templated, because they are structural metadata,
not secrets. `per-requester` cannot combine with `authProfileId`, and the
pair is remote-only: a `command` entry with either key aborts the load.
Overlapping `config` passthrough keys (`auth`, `oauth`, `oauth.*`) are
likewise a load error, so there is no silent precedence.

The boot registers the server and writes `mcp.servers.<name>.auth` /
`.oauth` via `config set --strict-json`. Credentials never live in the
spec or in `openclaw.json`. After the first boot, complete the flow once
from inside the running container:

```sh
docker compose exec agent openclaw mcp login docs   # prints the authorization URL (loopback callback)
docker compose exec agent openclaw mcp login docs --code abc123   # headless: paste the code from the redirect URL
```

OpenClaw stores and refreshes the tokens itself. While `auth: "oauth"` is
set, static `Authorization` headers on the same entry are ignored by the
runtime. `identity: "per-requester"` additionally requires
`gateway.publicOrigin` in the OpenClaw config (set it via a spec config
entry) so the OAuth callback URL is reachable.

### features

All four flags default to `false`.

- **`gh_auth`**: run `gh auth login --with-token` from `AGENT_GIT_TOKEN`
  on every boot. See [Quick start](#quick-start).
- **`gateway_auth`**: asserts the deployment arms gateway token auth via
  `OPENCLAW_GATEWAY_TOKEN`. The gateway reads that env var natively, and
  the env surface wins over any config reference (verified in the pinned
  gateway's credential-plan source), so the base writes no config pair.
  With the flag on and the token present, keys older base images wrote
  (`gateway.auth.token` + `secrets.providers.default`) are unset on the
  next boot when they exactly match the legacy pair and no env-active
  spec entry owns the path. When the token is absent, the flag warns
  naming the env var.
- **`plugin_prune`**: de-specified plugins the base installed are
  uninstalled on the next boot (ownership: `{data}/agent-managed-spec-plugins`).
  Operator installs are never touched, and enabling the flag later catches
  up on everything recorded while it was on.
- **`mcp_prune`**: the same contract for MCP servers (ownership:
  `{data}/agent-managed-mcp`). Default-off because that marker is
  agent-writable; without the flag, de-specified or foreign servers
  surface as per-boot orphan warnings instead of being unset.

### Worked examples

- `examples/spec.example.json`: one entry per feature, annotated by shape.
- `tests/fixtures/*grow-agent-like/spec.json`: local stdio servers with
  `--env` pairs, `split_csv` allowFrom, heartbeat entries behind `if_env`,
  a local plugin, `gh_auth` enabled.
- `tests/fixtures/*trade-agent-like/spec.json`: six servers mixing remote
  URLs (one key-templated query param, one bearer-token header) with local
  npx commands, `gh_auth` disabled, a different automation model.

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
| `/opt/agent/scripts/` | trigger scripts for `trigger-script:` headers | Read at boot by the cron reconciler | Read-only surface, sibling of `automations/`; content embedded into the job at seed time, never re-read at runtime. |

Docs living at `{data}/workspace/docs` is a hard standard: there is no
`{data}/docs` destination, and the entrypoint never writes one. Agents
migrating from a legacy `{data}/docs` layout move once, in their wrapper
entrypoint (see [Escape hatch: wrapper entrypoints](#escape-hatch-wrapper-entrypoints)),
before `seed_content` runs.

`AGENT_SKIP_SEED=1` skips content seeding only; reconciliation still runs.
Content is image-baked in every mode. There is no dev overlay and no
content bind mounts: the dev loop is rebuild-first (`docker/podman
compose up -d --build` reuses the cached COPY layers). The same rule keeps
automations unmountable in any mode, because writable cron prompt files
would be a self-modification surface for the agent.

## Boot sequence

`main()` runs the phases in order, then forks. Every phase is a plain
function in `container/entrypoint.py` and is individually callable from a
wrapper entrypoint.

1. **Load spec** (`load_agent_spec`): parse and template-resolve
   `spec.json` against the environment. Any violation aborts the boot
   before a single side effect.
2. **Upgrade prep** (`backup_before_upgrade`): when `AGENT_BASE_VERSION`
   changed since the last boot on a warm volume, run (in order) a raw
   pre-migration snapshot of the state database into the backups dir,
   `openclaw doctor --fix --non-interactive` (the pinned CLI, 2026.10+,
   gates both backups and gateway startup on state-db schema migrations),
   and `openclaw backup create --verify` into `/backups`; any failure
   aborts (see [Upgrades](#upgrades)). Fresh volumes just record the
   version; dev boots without the env var skip.
3. **First boot** (`first_boot_setup`, only when `{data}/openclaw.json` is
   absent): `openclaw setup --auth-choice <setup.auth_choice>`, register
   `model.fallback`, add each channel, install
   `@openclaw/llama-cpp-provider` (key-free local embeddings for memory
   search; this one is base-image behaviour, not spec-configurable).
4. **Reconcile** (when `AGENT_MANAGE_CONFIG=1`): `reconcile_config`, then
   `reconcile_mcp`, then `reconcile_plugins`. All idempotent; failures
   warn and never raise, so a flaky registration cannot take the gateway
   down. For `litellm-api-key` specs this phase also seeds
   `models.providers.litellm.baseUrl` (see
   [Model providers via LiteLLM](#model-providers-via-litellm)).

   `agents.defaults.models` is the model allowlist: once the map has
   entries, only those refs (plus `provider/*` wildcards and fallbacks)
   are allowed, and an absent or empty map means every catalog-visible
   model is allowed. The `openclaw setup` `zai-coding-*` auth choices
   seed it with a pinned list that lags the GLM release train
   (`zai/glm-4.7`, `zai/glm-5.2`), so a newer model, e.g. the current
   base default `zai/glm-5.3-flash`, gets rejected with "model not
   allowed" (cron `payload.model`, isolated runs). On every reconcile the
   base therefore merges the missing refs into an existing map: the
   current GLM 5.3 series (`zai/glm-5.3-flash`, `zai/glm-5.3`) for
   `zai-coding-*` specs, plus every spec-referenced model
   (`model.fallback`, `automations.model`; bare refs without a provider
   prefix are skipped). It never creates the map (that would newly
   restrict an allow-any deployment), never rewrites a non-map value
   (warns and stands down), and never touches it when an env-active spec
   config entry owns the path (`agents.defaults.models` or any
   `agents.defaults.models.*` key). To add models yourself, spec a config
   entry per key:

   ```json
   {"path": "agents.defaults.models.zai/glm-5.4", "value": {}}
   ```

   Owning any `agents.defaults.models.*` path opts the deployment out of
   the base merge: the spec then owns the allowlist entirely.
5. **gh auth** (`authenticate_gh`, only when `features.gh_auth` is true):
   `gh auth login --with-token` from `AGENT_GIT_TOKEN`.
6. **Seed** (`seed_content`): the table above, unless
   `AGENT_SKIP_SEED=1`.
7. **Fork.** The child runs `post_startup` (below) and exits. The parent
   runs `supervise` (`entrypoint.supervise`): the container CMD
   (`openclaw gateway`) spawns as a child in its own process group, and
   the entrypoint stays between tini and it, returning the CMD's exit code
   after graceful-shutdown drain (see
   [Graceful shutdown](#graceful-shutdown)). The child's `post_startup`
   tasks:

   - Wait for gateway health (polls `openclaw health` for up to 180s;
     timeout skips the rest, non-fatal).
   - Cron seeding: `seed_automations.main(["--model", <automations.model>])`
     in-process. Fail-closed: a bad or missing spec aborts before any
     mutation, and invalid `every:` durations or `cron:` expressions are
     load errors, not silent drift. Drift heals (see
     [Standardization decisions](#standardization-decisions)). Every CLI
     spawn carries a 60s timeout, so a hung cron call cannot hang the
     forked child. Seeded jobs run with a **bounded tool allow-list** by
     default (fs, runtime, web, and memory tools plus `bundle-mcp`; no
     `cron`, spawn, or browser tools, because a scheduled turn reaching
     the cron tool could self-replicate jobs). Override per job with a
     `tools:` header, or globally with `automations.default_tools` in the
     spec (`["*"]` restores unrestricted). Override the model per job
     with a `model:` header (falls back to `automations.model`); model
     drift, per-job or global, heals like any other field via
     `cron edit --model`. When `TELEGRAM_CHAT_ID` is set, seeded jobs
     also carry **failure alerts** routed to that chat
     (`--failure-alert` with include-skipped, attached via `cron edit`;
     `cron add` has no alert flags at the pinned base tag). Alert drift
     heals like any other job field. A job may also declare a
     `trigger-script:` header, a condition script the Gateway evaluates
     headlessly when the schedule is due, running the agent turn only on
     `fire: true`; see [Cron trigger scripts](#cron-trigger-scripts).
   - Memory reindex (unless `AGENT_MEMORY_REINDEX=0`): clear stale reindex
     locks, check status, then full rebuild, incremental pass, or skip.
     Three attempts with 10s backoff; a degraded success
     (`chunks_vec not updated` on stderr, vectors skipped) counts as
     retryable, because it is the memory-search-offline case.
   - Reconcile skill enablement against `openclaw doctor --lint`,
     stably. The check only examines *enabled* skills, so a disabled
     skill never re-flags and a cleared finding on a disabled skill is
     not proof of recovery. Disables therefore require the finding to
     persist across two doctor runs (the second is settle-spaced to ride
     out plugin/MCP pre-warm). Heals are proven by re-enabling first and
     re-checking; still-unavailable skills are disabled again in the same
     boot, and their retry is deferred until the image version changes
     (`{data}/doctor-heal-attempts`; delete the file to force a retry).
     Writes are batched (`config set --batch-json`): a transition boot
     costs at most two CLI writes, and a steady-state boot (unchanged env,
     converged verdicts) writes nothing and runs doctor exactly once.
     Skills the operator disabled by other means (no
     `{data}/doctor-disabled-skills` marker entry) are never touched.
   - Diagnostics (warn-only, never gates): the shared doctor run's
     summary is logged (`doctor: N finding(s)`) followed by up to ten
     per-finding lines (`doctor finding: <checkId> <path>`; message text
     is never logged, as it may quote values). The full report persists
     to `{data}/logs/doctor-report.json` when findings exist.
     `openclaw security audit --json` runs and persists
     `{data}/logs/security-report.json` the same way (same bounded
     detail lines). A boot summary (`{data}/status.json`: image version,
     warning count, completion time) closes post-startup; warning text
     never reaches disk, the status file carries counts only.
   - `openclaw config validate` and `openclaw doctor --lint`, warn-only:
     findings surface in logs, the gateway keeps running.

`--validate-spec` replaces all of the above with a dry parse (spec plus
automations directory) for CI.

## Cron trigger scripts

An automation can declare a `trigger-script:` header: a condition script
the Gateway evaluates each time the schedule is due. The job's agent turn
runs only when the script returns `fire: true`; otherwise the run is
skipped without run history. Evaluation state still persists, so scripts
can fire on *changes*: "alert only when the observed status differs from
the last evaluation".

```markdown
---
name: ci-watcher
every: 30m
deliver: announce
trigger-script: check-ci.js
---
Investigate the CI status change and summarize what moved.
```

```js
// scripts/check-ci.js — json() returns {fire, message?, state?}
const res = await tools.call('exec', { command: 'gh pr checks 123 --json state' });
const status = String(res?.result?.details?.aggregated ?? '').trim();
json({
  fire: status !== trigger.state?.status,
  message: `PR 123 CI: ${trigger.state?.status ?? 'unknown'} -> ${status}`,
  state: { status },
});
```

Contract, pinned at the base tag (source-verified in the image bundles
plus a live round-trip):

- The header value is a **relative path inside the scripts directory**
  (`/opt/agent/scripts`, or `AGENT_SCRIPTS_DIR`); no `..` and no absolute
  paths. The loader fails closed on missing, empty, non-UTF-8, or
  >64 KiB scripts (the CLI's own limit).
- The CLI reads the file at seed time and **embeds the trimmed bytes** as
  the job's `trigger.script`; the path never matters at runtime. Script
  content participates in currency: editing the file heals the job via
  one `cron edit --trigger-script`; dropping the header heals via
  `--clear-trigger`.
- Evaluation semantics (upstream): `json({fire, message?, state?})`
  return; `trigger.state` is the previous frozen state (16 KiB cap); each
  evaluation gets a 30s wall-clock budget and at most 5 tool calls; a
  returned `message` is appended to the agent-turn prompt. `once: true`
  (not exposed as a header) would disable the job after its first
  successful fired run.
- Triggers require `every`/`cron` schedules, and `every` must be at least
  30s (the Gateway's minimum trigger interval).

**Opt-in required.** Trigger evaluation runs headless with the **owning
agent's full tool policy, including `exec`**, not the job's bounded
`--tools` allow-list. That is unattended code execution, so the base never
arms it implicitly. Set

```yaml
environment:
  AGENT_AUTOMATION_TRIGGERS: "1"
```

and the cron seed arms the Gateway's `cron.triggers.enabled` config before
creating or editing any job. Without the env, a repo carrying a
`trigger-script:` automation aborts cron seeding loudly (no jobs are
created, edited, or pruned). The security decision stays with the
deployment, not with a header someone added to a markdown file. The
consumer mount story mirrors automations, read-only and never writable
(writable scheduled scripts would be an agent self-modification surface):

```yaml
volumes:
  - ./automations:/opt/agent/automations:ro
  - ./scripts:/opt/agent/scripts:ro
```

## Graceful shutdown

`supervise` keeps the entrypoint alive between tini and the container CMD.
The CMD runs as a child in its own process group, and a shutdown signal
starts a drain instead of an immediate teardown.

- **First `SIGTERM`/`SIGINT`** (`docker stop`, `compose stop`, Ctrl-C on
  an attached run): forwarded to the CMD pid only, never to its process
  group, so in-flight automations never see it. The gateway stops on its
  own terms; once it has exited, the supervisor waits for the gateway's
  process group (the automation processes; orphaned ones re-parent to
  tini, so the group is probed rather than waited on) to empty.
- **Grace bound** (`AGENT_SHUTDOWN_GRACE`, default 600s): whatever is
  still running when the grace expires is `SIGKILL`ed as a group. `0`
  skips the drain: forward, then force-kill at once.
- **A second signal force-kills immediately**, the operator escape hatch
  when a drain must not wait (a second Ctrl-C on an attached run).
- **Unprompted CMD exit** (crash): the process group is killed at once,
  identical to the pre-supervisor teardown, and the exit code propagates,
  so `restart: unless-stopped` fires promptly. Draining is a
  shutdown-signal courtesy, never a restart delay.
- **Exit code**: the CMD's own code when it exits, `128+N` when it died
  of signal N (a grace expiry surfaces as 137).

The engine must not cut a drain short: the stop timeout
(`stop_grace_period` in compose, `--stop-timeout` on `docker run`/`docker
stop`) must exceed `AGENT_SHUTDOWN_GRACE`, because the engine `SIGKILL`s
PID 1 when it expires and nothing survives that. The production compose
template sets `stop_grace_period: 11m` against the 600s default.

Boundary: only processes in the CMD's process group are drained. Every
spawned automation inherits it, so normal operation is covered. Anything
that `setsid`s itself away escapes the drain and dies at container
teardown.

## Standardization decisions

| Decision | Why |
| --- | --- |
| `AGENT_*` prefix replaces the legacy per-project names | One vocabulary across projects; the base cannot accidentally special-case one agent's names. |
| `TELEGRAM_CHAT_ID` is base-standard | Cron delivery needs one chat target the reconciler can read directly. All other `TELEGRAM_*` names stay project-side in spec refs (`TELEGRAM_BOT_TOKEN`, `TELEGRAM_ALLOWED_USERS`, `TELEGRAM_TOPIC_*`). |
| Docs at `{data}/workspace/docs`, never `{data}/docs` | Docs are workspace-adjacent reference material seeded every boot. Migrating agents move once in a wrapper entrypoint. |
| `automations.model` is required per project | A baked default would silently drift models between agents sharing one image. No default, no drift. Per-job `model:` headers are overrides, not defaults; the global stays explicit. |
| Model/provider configuration lives on a LiteLLM sidecar (recommended shape) | The agent process never holds provider keys: one proxy key in, all routing on the proxy (`litellm/config.yaml`). Model changes are a config review + restart, not an image rebuild. Direct provider auth (`zai-coding-*`) remains supported for single-provider deployments. |
| Seeded jobs get a bounded tool allow-list | A scheduled turn reaching the `cron` tool can self-replicate jobs (OWASP ASI06); the base default excludes recursion, spawn, and browser tools. Per-job `tools:` / spec `automations.default_tools` / `["*"]` escape hatches keep this the operator's call. |
| Cron trigger scripts sit behind `AGENT_AUTOMATION_TRIGGERS=1` | Trigger evaluation runs headless with the owning agent's full tool policy (including `exec`), not the job's `--tools` list. Arming that gateway-wide just because a header appeared in a markdown file would weaken the default posture; the env opt-in keeps unattended exec an explicit deployment decision. |
| Base sets `tools.deny` (cron, subagents, sessions_spawn, nodes) unless the spec configures `tools.*` | The same recursion/spawn surfaces are denied for the agent's own turns; `heartbeat_respond` stays allowed so heartbeats work. Any spec entry under `tools.*` signals operator ownership and the base default stands down. |
| Cron reconcile heals drift | Stored schedule shapes are compared precisely; anything legacy or foreign is treated as drift and healed with exactly one idempotent `cron edit`. A missing or broken spec aborts the whole run rather than pruning live jobs. |
| Automations are image-baked, never host-mounted | Writable cron prompt files would let the agent rewrite its own schedules. |

### Persona file trust model

Workspace persona files (`AGENTS.md`, `SOUL.md`, `MEMORY.md`, `USER.md`)
are agent-writable by design: OpenClaw injects them as prompt context, and
the agent is expected to maintain its own memory and journal. The base's
guardrails around this surface:

- Automations are image-baked and never writable; spec changes go through
  a rebuild + review.
- Seeded skills and docs are replaced wholesale every boot; drift heals on
  restart.
- The tool defaults above keep scheduled turns off the self-replication
  paths.

For tamper-evidence beyond that, keep `{data}/workspace` in a private git
repository (the agent commits its own history): the volume is the
authoritative record, and the verified upgrade backup snapshots it on
every image bump.

## Runaway and budget controls

Scheduled agents can loop (retry storms, self-reinforcing prompts), and an
unattended loop burns provider credits until a human notices. The control
surface today, strongest first:

1. **Set provider-side billing caps.** This is the real backstop. A hard
   monthly cap (and alert thresholds) on the provider account makes the
   worst case a paused key, not a bill.
2. **Use the bounded tool allow-lists** (in this base). Seeded jobs run
   without exec-free-for-all tooling by default, and recursion/spawn tools
   are denied, so a loop cannot self-replicate jobs or spawn fan-outs.
3. **Watch the failure alerts** (in this base). Every failed or skipped
   seeded run alerts the chat, so a retry storm is visible within one
   interval.
4. **Rely on trigger-script budgets** (upstream, at the pinned tag). Each
   condition evaluation is capped at 30s wall-clock and 5 tool calls,
   `fire: false` skips without run history, and the whole surface needs
   the `AGENT_AUTOMATION_TRIGGERS=1` opt-in to exist.
5. **Pass `--light-context`** where a job allows it. It exists on
   `openclaw cron add` at the pinned tag for cheaper turns; pass it per
   job via a wrapper if needed.

What does not exist yet: per-automation dollar/iteration caps enforced by
the runtime. OpenClaw has no budget surface at the pinned tag and no
upstream tracking issue for one. This section is the interim documentation
and will be replaced when upstream ships a knob the spec can surface.

## Python tools (optional)

The base image is stdlib-only Python by contract; no pip layer ships by
default. Consumers that need Python tools have two paths:

**Registry packages (build-arg passthrough).** Build with
`--build-arg OPENCLAW_IMAGE_PIP="<pkg>[==<ver>] ..."`. The base installs
`python3-pip` and the packages in a single layer only when the arg is set
(mirrors the OpenClaw official image's build-arg of the same name). The
entrypoint's own modules stay stdlib-only regardless.

```dockerfile
FROM ghcr.io/tankdonut/agent-base:2026.08.24.1
ARG OPENCLAW_IMAGE_PIP="textual==0.89.1 rich"
```

**Local-source tools (consumer recipe).** COPY the source and pip-install
it in your Dockerfile; the passthrough cannot express a path.

```dockerfile
# hadolint ignore=DL3002
USER 0:0
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3-pip \
    && rm -rf /var/lib/apt/lists/*
COPY tools/my-tool /tmp/my-tool
RUN pip3 install --break-system-packages --no-cache-dir /tmp/my-tool \
    && rm -rf /tmp/my-tool
USER 1000:1000
```

Prefer the build-arg when the tool is on a registry; keep the recipe for
vendored or local sources.

## Escape hatch: wrapper entrypoints

Projects with one-off needs do not get hooks or plugin loading in the
base. They write a wrapper entrypoint: import the phases, run what you
need, delegate the rest. Phases you can call directly include
`load_agent_spec`, `first_boot_setup`, `reconcile_config`, `reconcile_mcp`,
`reconcile_plugins`, `authenticate_gh`, `seed_content`, `post_startup`,
`supervise`, and `main`. A minimal wrapper that adds a one-time legacy
docs move and otherwise boots standard:

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
replaces the stock boot entirely. Interleaving custom steps between phases
works the same way, since each phase is a plain function over
`(spec, env)`. `main` returns the CMD's exit code once the supervised
gateway exits, so a wrapper that delegates to it stays in charge through
the drain.

## Image versioning

Images publish to `ghcr.io/tankdonut/agent-base` under date tags
(`YYYY.MM.DD`, e.g. `ghcr.io/tankdonut/agent-base:2026.08.23.1`) as
multi-arch manifests (linux/amd64 and linux/arm64, built on native
runners). There is no floating `latest`: agent containers are long-running,
and their boot behaviour should change only when a human bumps the pin.
Pin the exact tag in every project `FROM` and bump it deliberately. Date
tags make the diff reviewable and a bad bump reversible.

Published tags are immutable. A second release on the same day appends a
run suffix (`2026.08.23.1`, then `.2`) rather than replacing the morning
tag; each suffix gets its own image and GitHub release. The release-time
refs gate (`scripts/check-image-refs.sh`) validates both shapes, so a doc
referencing an unpublished suffix fails the release.

## Supply chain

Every tag release ships with a SLSA build-provenance attestation (pushed
to GHCR by CI) and an SPDX SBOM (attached to the GitHub release). Verify a
base image before you build on it:

```sh
gh attestation verify oci://ghcr.io/tankdonut/agent-base:<tag> \
  -R tankdonut/agent-base
```

Consumers that want bit-exact reproducibility can pin by digest instead of
tag. Every release's notes carry the digest and a copy-paste pin line. For
automated updates, Renovate's `dockerfile` manager with `pinDigests:
true` rewrites `FROM …:<tag>@sha256:…` and opens digest-bump PRs on new
releases.

### Deployment hardening (compose)

These are deployment-layer choices (compose/systemd), not image features.
The block below is the recommended baseline for home-server deploys:

```yaml
services:
  agent:
    security_opt:
      - no-new-privileges:true
    cap_drop: [ALL]
    # read_only plus tmpfs keeps the container filesystem immutable;
    # named volumes already cover every writable path the base needs
    # ({data}, /backups).
    read_only: true
    tmpfs:
      - /tmp
```

Add egress filtering at the network layer when the agent's outbound
surface is known (allowlist the provider/registry hosts).

`examples/compose.prod.agent.yml` is the complete production compose file
built on this baseline, and `docs/deployment.md` is the full deployment
playbook: host prep, TLS proxy, updates/rollback, backups, watchdogs, and
per-platform (Render/Fly/AWS/Raspberry Pi) notes.

## CI pattern

Downstream projects compose CI from `tankdonut/github-actions` rather than
hand-rolling workflows:

- lint/test job: the `pre-commit` action plus `setup-python-uv`, running
  the repo's hooks and `python3 -m unittest discover -s container`;
- a `--validate-spec` step against the project image, so a spec
  regression fails the build, not the boot;
- publish job: `build-and-publish-image.yaml` via `workflow_call`,
  emitting the date-tagged image.

This repo runs the same pattern it prescribes. The reusable pieces live in
the actions repo; projects reference them instead of copying YAML.

## Extension checklist

1. Copy `examples/env.example` to `.env` (or `secrets/agent.env`) and keep
   only the variables your spec references; the base vars can stay
   commented defaults.
2. Write `spec.json` starting from `examples/spec.example.json`; consult
   the fixtures for local-MCP versus remote-MCP shapes.
3. Create `workspace/` from `examples/workspace/` (`AGENTS.md`, `SOUL.md`,
   `USER.md`, `MEMORY.md`) and fill in the persona placeholders.
4. Assemble `skills/` and `docs/` as image-baked content; remember skills
   and docs are replaced wholesale every boot.
5. Write `automations/*.md` (name must match the file stem; exactly one of
   `every` or `cron`; `deliver`; optional `topic-env`; optional `tools`,
   a comma-separated tool allow-list where `*` opts a job back to
   unrestricted; optional `model`, a per-job model override that beats
   `automations.model` for that job alone; optional `trigger-script`, a
   condition script path inside `scripts/`, which additionally needs the
   `AGENT_AUTOMATION_TRIGGERS=1` deployment opt-in; see
   [Cron trigger scripts](#cron-trigger-scripts)).
6. Ship trigger scripts (if any) as `scripts/`, image-baked or mounted
   read-only at `/opt/agent/scripts`.
7. Write the thin Dockerfile per Quick start, pinning the current date tag.
8. Add the `agent` service from `examples/compose.agent.yml`.
9. Stand up CI per the pattern above, including the `--validate-spec` gate.

Validate early: `docker run --rm --env-file .env <image> --validate-spec`
catches schema, templating, and automation errors in seconds.
