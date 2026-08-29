# agent-base

Shared agent implementation and container (`agent-base`). Projects extend
the base image with a thin Dockerfile + a declarative `spec.json`; all boot
logic lives here.

## Status

Active. The full agent-base contract (spec schema, env vars, seed
lifecycle, boot sequence, extension checklist) lives in
[`docs/standard-agent.md`](docs/standard-agent.md).

## Layout

| Path | Purpose |
| ----- | ------- |
| `container/` | Base-image Python modules (entrypoint, spec loader, automations reconciler) + unittest suites |
| `cmd/` | `agentctl` CLI — scaffolds new downstream agent repos |
| `internal/` | agentctl scaffold engine + embedded templates |
| `templates/` | Project-facing templates: spec example, env contract, compose snippets, workspace skeletons |
| `docs/` | The standard-agent contract + migration guides |
| `scripts/` | Smoke harness |
| `make.sh` | Task runner (test / lint / smoke / build / push) |

## Quick start

```sh
./make.sh test    # python3 -m unittest discover container
./make.sh lint    # pre-commit run --all-files
```

## agentctl

`agentctl` is the operator CLI for downstream agent projects — scaffolding is
its first feature:

```sh
go run ./cmd/agentctl init ../my-agent   # scaffold a new agent repo
go install ./cmd/agentctl                # then, in any project:
agentctl up                              # lifecycle: dev/down/logs/rebuild/update
agentctl secrets init                    # secrets: init/check/edit
agentctl validate                        # spec gate via the base image
```

See `agentctl help` for the full command surface.
