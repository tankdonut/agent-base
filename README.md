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
| `examples/` | Image-contract examples: spec golden, env contract, compose snippets, workspace skeletons |
| `docs/` | The standard-agent contract + migration guides |
| `tests/` | E2E surface: smoke (fixture boots + drain), CLI contract gate, fixtures, openclaw shim |
| `scripts/` | `check-image-refs.sh` (release-time GHCR tag gate) |
| `make.sh` | Task runner (test / lint / smoke / build / push) |

## Quick start

```sh
./make.sh test    # python3 -m unittest discover container
./make.sh lint    # pre-commit run --all-files
```

## agentctl

`agentctl` is the operator CLI for downstream agent projects — scaffolding,
the local dev loop, and platform-targeted deployment (compose is the
reference platform):

```sh
go run ./cmd/agentctl init ../my-agent   # scaffold a new agent repo
go install ./cmd/agentctl                # then, in any project:
agentctl deploy                          # ship the checked-out tree (check → build → up)
agentctl dev up                          # local loop: hot-reload overlay
agentctl status · logs · stop · start · destroy
agentctl platform ls · set · check       # deployment platform management
agentctl doctor                          # pre-flight report (incl. litellm shape + real-image spec gate)
agentctl secrets init                    # secrets: init/check/edit
agentctl validate                        # spec gate via the base image
```

See `agentctl help` for the full command surface.
