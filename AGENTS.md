# Agent Instructions

## Commands

| Task | Command |
| ----- | ------- |
| All tests | `./make.sh test` |
| One module | `python3 -m unittest discover -s container -p "test_spec.py"` |
| Lint | `./make.sh lint` |
| Image smoke | `./make.sh smoke` |
| Build image | `./make.sh build` |

## Key Conventions

- Python 3.11 floor (bookworm image), stdlib only; no 3.12+ syntax; full type annotations; `unittest` + `mock`, never pytest.
- Loader modules fail closed: unknown key/token, ambiguous shape → abort loudly; never a silent empty string or skip.
- Secrets flow only through `{env:VAR}` spec refs and env vars; resolved values must never reach logs.
- `container/` files are the image contract; renaming/moving any of them changes downstream projects' Dockerfiles — update `docs/standard-agent.md` in the same commit.
- CI composes reusable actions from `tankdonut/github-actions` (`pre-commit`, `setup-python-uv`, `build-and-publish-image.yaml`); do not hand-roll equivalents.

## External References

| Need | File |
| ----- | ----- |
| Agent contract + project extension guide | `docs/standard-agent.md` |
| Migration guides (Freya, Mimir) | `docs/standard-agent.md#migrations` |
| Spec schema golden example | `templates/spec.example.json` |
