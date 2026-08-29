# Deploying a standard agent

`docs/standard-agent.md` is the image contract: what the container does at
boot and what it expects from the environment. This document is everything
*outside* the image — host prep, the production compose file, TLS and auth
in front of the gateway, update/rollback procedure, backups, watchdogs, and
platform-specific playbooks. The primary target is a single Ubuntu host with
docker compose; alternative platforms follow.

| If you are deploying on… | Read | Verdict |
| --- | --- | --- |
| Ubuntu + docker compose (default) | this whole document | the supported path |
| Raspberry Pi 5/4 (arm64) | [Raspberry Pi](#raspberry-pi) + this document | supported, with caveats |
| Render / Fly.io | [Render](#render) / [Fly.io](#flyio) | workable; snapshot-based durability |
| AWS | [AWS](#aws) | EC2+compose cheapest-correct; ECS Fargate viable |
| Railway / Lightsail / App Runner | [Railway](#railway) / [AWS](#aws) | Railway weakest; Lightsail/App Runner blocked, see breakers |
| A container management plane | [Management planes](#management-planes) | Komodo conditional (multi-host), Watchtower no |

## Deployment model

Fixed facts the rest of this document assumes (from the image contract):

| Fact | Value |
| --- | --- |
| Image | `ghcr.io/tankdonut/agent-base:<YYYY.MM.DD>` — date tags only, immutable, multi-arch amd64/arm64, no `latest` |
| Data volume | `/home/node/.openclaw` (`{data}`, SQLite inside — never raw-copy while running) |
| Backup volume | `/backups` (override: `AGENT_BACKUP_DIR`; the CLI refuses output inside `{data}`) |
| Gateway | listens on 18789 (container); bearer-token auth; WebSocket traffic |
| Health | `/healthz` HTTP probe; HEALTHCHECK 30s/10s/3 retries, 300s start period |
| Lifecycle | PID 1 is tini → entrypoint, which supervises `openclaw gateway`: on stop, in-flight automations drain up to `AGENT_SHUTDOWN_GRACE` (600s default) before exit; `restart: unless-stopped` owns restarts |
| Upgrades | image tag bumps only; every version delta auto-runs a verified backup into `/backups` **before** mutating a warm volume, and a failed backup aborts the boot on purpose |

## Host prep (Ubuntu)

Install Docker Engine from Docker's apt repo (not the distro package), then
pin it:

```sh
sudo apt-get install docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin
sudo apt-mark hold docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin
```

Held packages are skipped by `unattended-upgrades`, so engine upgrades stay
deliberate. Configure `/etc/docker/daemon.json`:

```json
{
  "live-restore": true,
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "5" },
  "icc": false
}
```

- `live-restore` keeps containers running across `systemctl reload docker`
  and engine *patch* upgrades. It does not survive engine restarts that
  change the daemon config — apply config changes with
  `sudo systemctl reload docker` where possible, and treat minor-version
  engine upgrades as maintenance windows.
- `log-opts` are daemon-level defaults; the production compose file also
  sets them per-service, and container-level settings win.
- `icc: false` disables inter-container chatter on the *default* bridge.
  The agent lives on its own network anyway (next section); this hardens
  everything else you run.

Then `sudo systemctl enable --now docker`.

For private GHCR access: fine-grained PATs **cannot** authenticate to GitHub
Packages — create a classic PAT with `read:packages` and
`docker login ghcr.io --password-stdin < token-file`. On a headless host,
store the token in a credential store (`pass`/GPG) or a `chmod 600`
`~/.docker/config.json` rather than shell history.

## Production compose file

`templates/compose.prod.agent.yml` is the complete, ready-to-adapt file —
not a snippet. The directives it locks in, and why:

| Directive | Why |
| --- | --- |
| dedicated `agent-net` network | the gateway port is a bearer-auth-only surface; sibling containers sharing a network with the agent must be a deliberate choice (the reverse proxy), not an accident of `docker compose up` |
| both named volumes (`{data}` **and** `/backups`) | without the `/backups` volume, upgrade archives vanish with the container — the entrypoint still writes them, they just die on redeploy |
| `127.0.0.1:${AGENT_GATEWAY_PORT:-18789}:18789` publish | remote exposure only ever happens through your TLS proxy |
| healthcheck re-declared in compose | matches the image HEALTHCHECK explicitly; matters on platforms that ignore image metadata (ECS) and for `--wait` deployments |
| `stop_grace_period: 11m` | exceeds `AGENT_SHUTDOWN_GRACE` (600s default), so the engine's stop-timeout `SIGKILL` never cuts an automation drain short (docs/standard-agent.md "Graceful shutdown") |
| `security_opt: [no-new-privileges:true]`, `cap_drop: [ALL]`, `read_only: true`, tmpfs `/tmp` | named volumes already cover every writable path the base needs, so the root filesystem can be immutable and the capability set empty |
| `deploy.resources.limits` (cpus/memory/pids) | honored by compose v2 outside swarm; OOM kill exits tini, which trips `restart: unless-stopped`; keep the sum of all services' limits below host RAM |
| `logging: json-file` + rotation | keeps `docker logs --follow` working while bounding disk use |

Rootless Podman works too, with two caveats: named volumes can hit SELinux
MCS stale-category denials (add `label=disable` under `security_opt` — see
`templates/compose.agent.yml` for the rationale), and rootless cgroup
delegation grants `memory` + `pids` but **not** CPU limits unless you extend
the delegate list. Do not mix: pick rootful Docker or rootless Podman and
keep host docs consistent with it.

## Gateway auth and network exposure

The gateway is the control plane: its bearer token is root-equivalent
(operator-scoped tool invocation, cron, node pairing). Treat it like a root
password.

1. **Always configure auth.** The upstream gateway fails closed — no valid
   auth configured means it refuses connections beyond loopback — so a
   missing token is a lockout, not an exposure. But lockouts are still
   outages: set the token everywhere the gateway is reachable, before the
   first remote exposure. The gateway reads `OPENCLAW_GATEWAY_TOKEN`
   natively (the env var alone arms token auth); the canonical config key
   is `gateway.auth.token` — in a spec, `features.gateway_auth: true`
   writes the reference pair pointing at the env var. Never write the raw
   token as a config value: valid config keys persist **plaintext** to
   `openclaw.json` and ride every `/backups` archive (verified against the
   pinned CLI; `config get` redacts, the file does not).
2. **Header, never query string.** Non-browser clients can set
   `Authorization: Bearer …` — do that; tokens in WS query strings leak
   into proxy and gateway access logs.
3. **TLS terminates at the proxy**; the proxy forwards to
   `127.0.0.1:18789`. Layer optional extras (IP allowlist, mTLS) *at the
   proxy* — but do not stack basic auth on the same paths, it collides with
   the bearer header.

### Caddy (recommended: least ceremony)

```caddyfile
agent.example.com {
    reverse_proxy 127.0.0.1:18789
}
```

Automatic Let's Encrypt, WebSocket upgrade handled natively, and the
`Authorization` header passes through by default. Run Caddy as a host
process (systemd) so `127.0.0.1` is reachable; a containerized proxy must
instead reach the agent through the shared `agent-net` network or
`host.docker.internal` (`extra_hosts: host-gateway`).

### nginx

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    server_name agent.example.com;
    listen 443 ssl;
    # TLS: certbot --nginx or your usual pipeline

    location / {
        proxy_pass http://127.0.0.1:18789;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

`proxy_http_version 1.1` + the upgrade map are what make WebSockets work;
the read timeout must exceed your longest idle WS session (default 60s will
drop quiet agent sessions).

### Traefik

Label or file provider both work. Required shape: `exposedByDefault=false`,
an ACME `certificatesResolver` for TLS, and raised `respondingTimeouts`
(defaults read 60s / idle 180s are too short for long-lived WS). Do not put
`basicAuth` middleware on the agent route (bearer collision). The docker
provider needs `docker.sock:ro` mounted into Traefik — that socket is
docker-equivalent power, so weigh it against a two-line Caddyfile on the
host.

## Updates and rollback

The image contract already covers the in-container half: a version delta
triggers a verified `/backups` archive before any mutation, and a failed
backup aborts the boot (deliberately — data safety outranks availability
during a migration). Host-side, an update is:

```sh
# 1. Bump the FROM tag in the project Dockerfile
#    (2026.08.24 → 2026.08.27; a Renovate PR is the tidy way)
# 2. Rebuild and roll, waiting for the healthcheck
docker compose -f compose.yml build --pull agent
docker compose -f compose.yml up -d --wait --wait-timeout 420 agent
```

`--wait` blocks until the service reports healthy, which covers the 300s
start period plus the upgrade backup. Expected log lines: `Image changed
(<old> → <new>) — creating verified backup`, then the normal boot sequence.

Rollback is the same dance with the old tag: revert the Dockerfile bump,
rebuild, `up -d --wait`. Date tags are immutable, so the old base layers are
still local and nothing has moved underneath you; the version marker rides
the `{data}` volume, so the rollback re-runs its own backup first — by
design.

Verify an image before building on it (attestations ship with every tag
release):

```sh
gh attestation verify oci://ghcr.io/tankdonut/agent-base:2026.08.27 \
  -R tankdonut/agent-base
```

For automated bumps, Renovate's `dockerfile` manager understands the
`YYYY.MM.DD[.N]` scheme with `regex` versioning; `pinDigests: true` rewrites
`FROM …:<tag>@sha256:…` and opens digest-bump PRs — the strictest form of
pinning available.

## Backups and restore

Three layers, from cheapest to most durable:

1. **Upgrade backups (automatic, in-container).** Every version delta writes
   a verified archive to the `/backups` volume before touching `{data}`.
   This is the migration safety net, not a retention policy.
2. **Host volume snapshots (your cadence).** SQLite lives in `{data}`; a
   live raw copy is not a consistent backup. Stop the agent first:

   ```sh
   docker compose -f compose.yml down
   docker run --rm -v <project>_agent-data:/data -v "$PWD":/backup alpine \
     tar czf /backup/agent-data-$(date +%F).tgz -C /data .
   docker compose -f compose.yml up -d --wait
   ```

   Note the volume's real name is prefixed by the compose project name.
   Restore is the mirror image: `down`, `tar xzf` into a fresh volume, `up`.
3. **Off-host copies.** Whatever you do on the host, ship the `/backups`
   volume and the latest tarballs somewhere else. `gh` credentials live at
   `~/.config/gh` *inside the container but outside `{data}` — volume
   snapshots do not cover them; they are re-established from
   `AGENT_GIT_TOKEN` every boot, so there is nothing to back up, but expect
   a first-run auth log line on every redeploy.

## When healthy stays down: watchdog

Docker restarts on *exit*, never on *unhealthy* — a gateway that is up but
sick (healthcheck failing, container still running) stays that way
indefinitely. On an Ubuntu host, a systemd timer is the cheapest honest
watchdog:

```ini
# /etc/systemd/system/agent-unhealthy-watchdog.service
[Unit]
Description=Restart unhealthy agent container

[Service]
Type=oneshot
ExecStart=/bin/sh -c "docker ps -q -f health=unhealthy | xargs -r docker restart"
```

```ini
# /etc/systemd/system/agent-unhealthy-watchdog.timer
[Unit]
Description=Run agent health watchdog every 5 minutes

[Timer]
OnCalendar=*-*-* *:0/5
Persistent=true

[Install]
WantedBy=timers.target
```

`systemctl enable --now agent-unhealthy-watchdog.timer`. Scope the filter
(`-f name=^<project>-agent`) once you run more than one container. The
alternative is an `autoheal`-style sidecar, but it needs the docker socket
mounted — docker-equivalent power for a restart loop is a bad trade on a
single-agent host.

## systemd integration (optional)

Compose restart policies own crash recovery; systemd can own the *host*
lifecycle — boot ordering, `journalctl`-visible status, a unit other
services can `Require`. Keep it thin and never set `Restart=` here (that
fights Docker's own policy):

```ini
# /etc/systemd/system/<project>-agent.service
[Unit]
Description=<project> agent (docker compose)
Requires=docker.service
After=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=/opt/<project>
ExecStart=/usr/bin/docker compose up -d --wait
ExecStop=/usr/bin/docker compose down
TimeoutStartSec=600

[Install]
WantedBy=multi-user.target
```

## Monitoring

- Liveness: the healthcheck state (`docker inspect --format '{{.State.Health.Status}}' <container>`), `/healthz` from your watchdog or an uptime monitor (Uptime Kuma runs fine on arm64 alongside).
- Boot summary: `{data}/status.json` (image version, warning count, completion time) written every boot — ship it with whatever log/agent collector you already run.
- Logs: `docker logs --follow` preserves the entrypoint's phased boot output; secrets never appear in them by contract (loader warnings name keys, not values). CLI stderr from `gh`/memory/cron phases does pass through to container logs — operator-level access only.
- Volume drift: `{data}` is agent-writable and trusted at boot. Periodic tar snapshots (the backup step above) double as integrity monitoring; diff the file lists to notice marker-file or config tampering early.

## Security checklist (deployment layer)

- [ ] `OPENCLAW_GATEWAY_TOKEN` set via the canonical `gateway.auth.token` spec path, and the boot log confirms auth applied — never rely on the gateway key alone (the upstream gateway fails closed, so misconfiguration surfaces as lockout: test remote access once before depending on it).
- [ ] Token treated as root-equivalent: header-only transport, no query strings, no copies in proxy logs.
- [ ] Dedicated compose network; only the proxy shares it or reaches the port.
- [ ] `read_only`, `cap_drop: [ALL]`, `no-new-privileges` active (template defaults).
- [ ] Image provenance verified (`gh attestation verify` or digest pinning via Renovate `pinDigests`).
- [ ] `/backups` volume declared; backup aborts fail loudly (exit 1 + marker retry), so an aborted boot is a *feature* — investigate, don't force past it.
- [ ] Host: docker packages held, daemon log rotation set, ghcr token in a credential store.
- [ ] Egress allowlisting at the network layer once the outbound surface is known (provider/registry hosts).

## Crash-loop diagnosis

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Boot dies before the gateway, traceback naming `skills`/`docs` and `rmtree` | a symlink inside `{data}/skills/` or `{data}/workspace/docs/` — seeded dirs are replaced wholesale and `rmtree` refuses to cross symlinks | `docker run --rm -it -v <project>_agent-data:/data alpine sh` → replace the symlink with a real directory |
| `upgrade backup failed — aborting boot` repeats | `/backups` volume missing/full/unwritable | check the volume mount and free space; the version marker intentionally retries until the backup succeeds |
| Container runs, healthcheck never goes healthy, logs stop after seeding | gateway never came up (auth/env problem upstream) | check `docker logs` for gateway errors; verify token config; `openclaw health` inside the container |
| Remote clients get refused, local curl works | proxy not forwarding WS or wrong port | proxy config sections above; test `curl -H "Authorization: Bearer …" https://host/healthz` |
| Lockout after a token rotation | clients hold the old token; spec `{env:}` ref means the value is read at boot | rotate in `.env` + redeploy + update clients in one window |

## Multi-agent on one host

One compose project per agent, nothing shared — by design, agents are not
each other's trust domain. Exactly two resources must be unique
host-wide; everything else derives from them.

| Resource | How it stays unique |
| --- | --- |
| Compose project name | The scaffolded `compose.yml` pins a top-level `name:` (the lowercased project name); it prefixes the containers, volumes, and `agent-net` network, and keeps them stable across directory renames. Hand-rolled stacks get the same by setting `name:` explicitly (or exporting `COMPOSE_PROJECT_NAME`). |
| Host gateway bind | A distinct `AGENT_GATEWAY_PORT` per agent, in each project's `.env`. The container-internal port stays 18789; only the host bind moves. |

`agentctl up`/`dev` probe the resolved loopback port before starting and
warn when it is already bound — either this stack is already up (ignore)
or another agent owns the port and the new one needs its own
`AGENT_GATEWAY_PORT`.

The rest of the playbook:

- **Reverse proxy:** one site block per agent domain, each forwarding to
  its agent's loopback port:

  ```caddyfile
  freya.example.com {
      reverse_proxy 127.0.0.1:18789
  }
  mimir.example.com {
      reverse_proxy 127.0.0.1:18790
  }
  ```

- **Watchdog:** scope the restart filter per project —
  `docker ps -q -f health=unhealthy -f name=^<project>-agent` — or run
  one timer per agent ([watchdog](#when-healthy-stays-down-watchdog)).
- **systemd:** one `<project>-agent.service` per compose project
  ([systemd integration](#systemd-integration-optional)).
- **Resources:** keep the sum of all agents' `deploy.resources.limits`
  below host RAM; each agent keeps its own ceiling.
- **Backups:** volumes are per-project (`<project>_agent-data`,
  `<project>_agent-backups`); the [snapshot
  procedure](#backups-and-restore) runs unchanged, once per project.
- **Re-scaffolding over an older project:** project-name pinning changed
  real volume names from `<dir>_<project>-agent-data` to
  `<project>_agent-data`. After `agentctl init --force` + `up`, either
  copy the old volume's contents into the new one or keep the previous
  compose.yml — an empty new volume silently re-runs first boot.

## Alternative platforms

The contract's hard requirements anywhere: persistent writable storage at
`/home/node/.openclaw`, a UDP/TCP path to the gateway port, health checking
that tolerates a 300s cold start, and deliberate (not automatic) image
bumps. Pricing below was checked Aug 2026 and will drift.

### Render

The best PaaS fit. Disk mounts directly at `/home/node/.openclaw`
(SQLite-safe, encrypted at rest, daily snapshots retained ≥7 days), deploys
from a private GHCR image with registry credentials, WebSocket traffic
works, and the deploy health window (15 min) comfortably covers the 300s
start period. Running services that fail health checks for 60s are restarted
natively — no sidecar watchdog needed.

- Set the service's env to align ports: `OPENCLAW_GATEWAY_PORT` must equal
  the port Render expects the container to listen on (Render assigns
  `PORT`; on Docker services the default expectation is 10000). The gateway
  reads `OPENCLAW_GATEWAY_PORT` at startup, so one env var aligns both
  sides.
- One caveat: a Render service takes its persistent disk(s) per service,
  and the only mount point that matters is `{data}` — the default `/backups`
  lands on the ephemeral layer, so upgrade archives evaporate on redeploy.
  Rely on Render's disk snapshots for cross-deploy durability (they are
  daily; run manual `tar` exports via a shell if you need tighter RPO).
- Image-backed services have no auto-deploy; wire the tag bump to a deploy
  hook with the new `imageURL` when you want push-button updates.
- Shape: Starter 512 MB / $7/mo plus disk at ~$0.25/GB.

### Fly.io

Best tiny-VM option. One encrypted volume per machine — mount it at
`/home/node/.openclaw`, set `internal_port = 18789`, `restart.policy =
"always"`, disable auto-stop (`min_machines_running = 1`) since an agent is
not a request-scaled web app. Daily volume snapshots (5-day retention) are
again the `/backups` story: one volume per machine means the backup
directory stays ephemeral. WebSockets fine. Shape: shared-cpu-1x 256 MB in
`ams` ≈ $3.32/mo plus volume at ~$0.15/GB (no free allowance for new orgs).

### Railway

Weakest fit. Non-root UID volume permissions push you toward
`RAILWAY_RUN_UID=0`, which breaks the runs-as-node contract; private
registry auth is Pro-gated; the default health check is deploy-time-only
(300s). Workable for a hobby instance; not recommended for the primary.

### AWS

- **EC2 + compose (cheapest correct path).** A `t4g.small` (ARM, 2 GB) is in
  the free tier through 2026-12-31; afterwards see the AWS calculator
  (per-size pricing changes — never quote a blog number). Force IMDSv2
  (hop limit 2), use SSM Session Manager instead of SSH keys, gp3 root
  volume. Everything in this document applies verbatim — it *is* the Ubuntu
  playbook, on Graviton. Add EBS snapshots (`{data}` lives in a Docker
  volume on the root disk) via DLM lifecycle rules.
- **ECS Fargate (viable with caveats).** Persistent state needs EFS with an
  access point: `PosixUser` uid/gid `1000` exactly and `CreationInfo`
  `755`/`1000`/`1000`, mounted at `/home/node/.openclaw`. Mount a second EFS
  target at `/backups` or upgrade archives die with the task. ECS **ignores
  image HEALTHCHECK metadata** — re-declare the health check in the task
  definition, and set both `healthCheckGracePeriodSeconds` (ALB) and the
  check's `startPeriod` (capped at 300s — zero headroom, budget carefully).
  Front it with an ALB with a raised idle timeout (default 60s kills quiet
  WebSockets; 3600s is safe) and enable the deployment circuit breaker with
  automatic rollback. Shape: ARM 0.25 vCPU/1 GB ≈ $9/mo plus the ALB
  (~$16/mo) — the ALB dominates; ECS on EC2 or a plain EC2 host beats it
  for a single agent.
- **Lightsail Containers: blocked.** No persistent volumes — non-starter.
- **App Runner: blocked, three times over.** 3 GB ephemeral disk *including* the image,
  ECR-only images, 120s request cap.

### Raspberry Pi

Feasible on Pi 5 or Pi 4 with 4 GB+ (arm64); Zero 2 W, Pi 3, and any 32-bit
OS are blocked by memory/architecture. Prefer Ubuntu Server 24.04 arm64 or
Pi OS Lite 64-bit.

- **Storage:** boot from NVMe (Pi 5: `BOOT_ORDER=0xf416`) or USB SSD; SD
  cards wear out under SQLite + Docker journal churn. A read-only rootfs is
  incompatible with overlay2 — give `/var/lib/docker` and `{data}` a
  writable home instead.
- **Power:** Pi 5 wants 5V/5A; undervoltage throttling shows up as mysterious
  slowness under load. Budget a proper PSU (PoE HATs rated 5V/5A work).
- **Docker config:** the daemon.json log rotation from host prep is not
  optional here — `log2ram`-style tools cover `/var/log` only, never
  `docker logs`. Set a hardware watchdog `RuntimeWatchdogSec=10` (values
  above 15s reboot-loop on Pi watchdog hardware).
- **First boot:** the base installs `@openclaw/llama-cpp-provider` on first
  boot; ubuntu-arm64 prebuilts exist, but treat the first boot on a new Pi
  image as a smoke test before calling the deployment done — a missing
  prebuilt aborts setup loudly (fail-closed), and you want to learn that in
  testing, not at 03:00.

## Management planes

- **Watchtower: not applicable.** It follows a *moving tag or digest* and
  replaces the container; this image's tags are immutable date stamps — the
  thing that must change is the tag reference in your Dockerfile, which
  Watchtower cannot do. Renovate (or any PR bot) bumping the `FROM` line is
  the correct automation. (Watchtower is also archived, as of Dec 2025.)

### Komodo (v2.3.2, checked Aug 2026)

[Komodo](https://github.com/moghtech/komodo) is the one management plane
that fits this workload's update model. Facts, source-verified against the
pinned release:

| Question | Answer |
| --- | --- |
| What it adds | Core (UI + API + mandatory DB: FerretDB/Postgres or Mongo) plus Periphery, a stateless agent per host; stacks deploy from a git repo with a push webhook |
| Compose support | your compose file runs **verbatim** — Periphery shells out to plain `docker compose -f … up -d`; nothing is parsed or stripped, so limits/healthcheck/logging/networks all apply |
| Update fit | Komodo's auto-update watches a *same tag's digest* — useless for immutable date tags. The right loop is Renovate tag-bump PR → merge → git webhook auto-redeploy ("merging is deploying"), a pattern Komodo's own docs point pinned-tag users to |
| Healthcheck | **blind**: stack state maps container status (running/exited), never docker `Health` — the [watchdog](#when-healthy-stays-down-watchdog) stays necessary |
| Alerting | state-change alerts only; no native Telegram (Custom webhook/ntfy/Slack/Discord/Pushover) |
| Footprint | ~300 MB RAM + 1 core for the whole control plane (core ~35 MB, periphery ~40 MB, DB <250 MB); arm64 multi-arch images |
| Security cost | Periphery mounts `docker.sock` + `/proc`, terminals/exec on by default (disable flags exist), and a database to operate — docker-equivalent blast radius on every managed host |

Verdict: over-engineered for a single host running one deliberate-update
container — compose + Renovate + `up -d --wait` already covers that. It
earns its keep when a second host joins (e.g. the Pi) or you want a unified
dashboard: run Core on the Ubuntu host, **outbound-mode systemd Periphery**
on each host (the agent dials home; no inbound ports), FerretDB, TOTP 2FA,
terminals disabled, Caddy in front. Two caveats if adopted: verify the
host's compose version (open Komodo issue #1128:
`deploy.resources.limits.memory` silently dropped on Docker Compose v5.0.1
hosts), and note deploys run `up -d` without `--wait` (addable via stack
`extra_args`), so health-gating stays your watchdog's job.

## Deployment checklist

1. Host prep done (packages held, daemon.json, ghcr login).
2. `.env` from `templates/env.example` — every `{env:}` ref your spec uses
   resolves; `OPENCLAW_GATEWAY_TOKEN` set.
3. `templates/compose.prod.agent.yml` adapted (project name, limits).
4. First boot verified: healthy within 300s, logs clean, token auth works
   through the proxy.
5. Backup tarball landed off-host once; watchdog timer enabled.
6. Update path rehearsed once (tag bump → `up -d --wait` → healthy) *before*
   you need it in anger.
