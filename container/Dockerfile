# agent-base — the standard-agent base image.
#
# Distilled from the two source agents this repo unifies:
#   - ../grow-agent/freya/Dockerfile       (local MCP + plugins + seeds)
#   - ../trade-agent/agent/Dockerfile      (remote MCP + every-boot reconcile)
#
# Projects extend this image with a thin Dockerfile that COPYs their
# spec.json (to /opt/agent/spec.json), automations (to /opt/agent/
# automations/), and seed content (to /opt/seed/{workspace,skills,docs}).
# All boot logic lives in the entrypoint — see docs/standard-agent.md.

# Pinned to the exact tag both source agents build from (node:24-bookworm-
# slim, USER node uid 1000, tini, openclaw CLI, gateway :18789).
FROM ghcr.io/openclaw/openclaw:2026.7.1-2

# Image version (OCI label + io.agent.base.version); date-tag default.
ARG AGENT_BASE_VERSION=2026.08.21

# The base image runs as the non-root `node` user (uid 1000); root is
# needed only for apt and /opt writes.
# hadolint ignore=DL3002
USER root

# --- Python (for the spec-driven entrypoint) ---
# The OpenClaw base is node:24-bookworm-slim (Debian, no Python). python3
# alone is enough: the container/ modules are stdlib-only by contract
# (AGENTS.md) — no pip, no venv, nothing to pin beyond the distro.
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 \
    && rm -rf /var/lib/apt/lists/*

# --- Agent modules (the image contract; paths are load-bearing) ---
# entrypoint.py imports seed_automations and spec as top-level modules —
# all three must sit in the same directory (/opt/agent).
COPY --chown=node:node container/entrypoint.py         /opt/agent/entrypoint.py
COPY --chown=node:node container/seed_automations.py   /opt/agent/seed_automations.py
COPY --chown=node:node container/spec.py               /opt/agent/spec.py

# automations default dir (script-dir sibling resolution in
# seed_automations.build_jobs) + seed placeholders. The placeholders stay
# EMPTY here: minimal projects may mount nothing over them, and
# entrypoint.seed_content copytree sources must exist regardless.
RUN mkdir -p /opt/agent/automations /opt/seed/workspace /opt/seed/skills /opt/seed/docs \
    && chown -R node:node /opt/agent /opt/seed

LABEL org.opencontainers.image.title="agent-base" \
      org.opencontainers.image.description="Standard OpenClaw agent base image: spec-driven entrypoint, config/MCP/plugin reconciliation, content seeding" \
      org.opencontainers.image.version="${AGENT_BASE_VERSION}" \
      org.opencontainers.image.base.name="ghcr.io/openclaw/openclaw:2026.7.1-2" \
      io.agent.spec-version="1" \
      io.agent.base.version="${AGENT_BASE_VERSION}"

# Entry logs must stream (docker logs --follow) before the gateway is up.
ENV PYTHONUNBUFFERED=1

# Drop back to the non-root node user for runtime.
USER node

# OpenClaw gateway WebSocket + HTTP port (health, API, agent sessions).
EXPOSE 18789

# tini as PID 1 (signal forwarding + zombie reaping, e.g. the forked
# post-startup child), entrypoint reconciles then execs CMD.
ENTRYPOINT ["tini", "--", "python3", "/opt/agent/entrypoint.py"]
CMD ["openclaw", "gateway"]
