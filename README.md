# agent-base

[![CI](https://github.com/tankdonut/agent-base/actions/workflows/ci.yml/badge.svg)](https://github.com/tankdonut/agent-base/actions/workflows/ci.yml)

One OpenClaw agent image, many agents. Downstream projects ship content — a
declarative `spec.json`, seed directories, cron prompts — and inherit the
whole boot pipeline. No project writes boot code.

## What the base image does

Every boot, `container/entrypoint.py` runs the full pipeline:

- Validates the spec fail-closed — unknown keys or ambiguous shapes abort
  the boot, never silently degrade
- Reconciles config, MCP servers, and plugins idempotently — drift heals,
  failures warn and never take the gateway down
- Seeds content: workspace on first boot, skills + docs replaced every boot
- Seeds cron from image-baked automations (never host-mounted) with drift
  healing and a bounded tool allow-list by default
- Handles model providers via an optional LiteLLM sidecar
- Drains gracefully on shutdown (SIGTERM → wait → force-kill) and takes a
  verified backup before any image upgrade
- Runs doctor: diagnostics, self-healing skill reconcile, boot summary

Design invariants:

- Secrets flow only through `{env:VAR}` refs and env vars; values never
  reach logs (locked by canary tests)
- Stdlib-only Python 3.11; no pip in the image
- Date tags only (`YYYY.MM.DD[.N]`), no `latest`; releases carry SBOM +
  build provenance attestations

## Repository layout

| Path | Purpose |
| ----- | ------- |
| `container/` | Base-image Python modules (entrypoint, spec loader, automations reconciler) + unittest suites |
| `examples/` | Image-contract examples: spec golden, env contract, compose snippets, workspace skeletons |
| `docs/` | The standard-agent contract + deployment guide + migrations |
| `tests/` | E2E surface: real-image smoke + drain, CLI contract gate, fixtures, openclaw shim |
| `scripts/` | `check-image-refs.sh` (release-time GHCR tag gate) |
| `make.sh` | Task runner |

## Developing the base

```sh
./make.sh test           # container unit suites
./make.sh lint           # pre-commit (ruff, hadolint, markdownlint)
./make.sh smoke          # real-image smoke: fixture boots + graceful-shutdown drain
```

## Documentation

- [`docs/standard-agent.md`](docs/standard-agent.md) — the full agent
  contract: spec.json reference, environment contract, boot sequence,
  LiteLLM setup, upgrades, extension checklist, migration guides
- [`docs/deployment.md`](docs/deployment.md) — deployment guide: host
  prep, proxy, platforms
- [`examples/`](examples/) — golden `spec.example.json`, `env.example`,
  compose templates, LiteLLM proxy config
- [`AGENTS.md`](AGENTS.md) — repo conventions for coding agents and
  contributors
