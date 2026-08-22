# agent-base — Standard OpenClaw Agent Base Image

Shared agent implementation and container (`agent-base`) distilled from Freya
(grow-agent) and Mimir (trade-agent). Projects extend the base image with a
thin Dockerfile + a declarative `spec.json`; all boot logic lives here.

## Status

Active. The full agent-base contract (spec schema, env vars, seed
lifecycle, boot sequence, extension checklist) lives in
[`docs/standard-agent.md`](docs/standard-agent.md). Migration guides for
Freya and Mimir land there as those projects move onto the image.

## Layout

| Path | Purpose |
| ----- | ------- |
| `container/` | Base-image Python modules (entrypoint, spec loader, automations reconciler) + unittest suites |
| `templates/` | Project-facing templates: spec example, env contract, compose snippets, workspace skeletons |
| `docs/` | The standard-agent contract + migration guides |
| `scripts/` | Smoke harness |
| `make.sh` | Task runner (test / lint / smoke / build / push) |

## Quick start

```sh
./make.sh test    # python3 -m unittest discover container
./make.sh lint    # pre-commit run --all-files
```
