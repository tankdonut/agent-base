#!/usr/bin/env bash
# License allowlist gate: every Go module dependency must be on the
# permissive list. GPL/AGPL code must never be vendored into the
# agentctl binary or its embedded assets — the plane's AGPL tools
# (Grafana, Loki) run as unmodified external images, which is the
# documented boundary (docs/standard-agent.md "The plane").
set -euo pipefail
cd "$(dirname "$0")/.."

allowed=(
  "github.com/BurntSushi/toml"
  "github.com/cpuguy83/go-md2man/v2"     # MIT — indirect via cobra (man docs)
  "github.com/inconshreveable/mousetrap"
  "github.com/russross/blackfriday/v2"   # BSD-2 — indirect via go-md2man
  "github.com/spf13/cobra"
  "github.com/spf13/pflag"
  "github.com/tankdonut/agent-base"
  "go.yaml.in/yaml/v3"
  "gopkg.in/check.v1"                    # gocheck (permissive) — yaml.v3 test dep
  "gopkg.in/yaml.v3"
)

status=0
while read -r mod _; do
  [ -z "$mod" ] && continue
  ok=0
  for a in "${allowed[@]}"; do
    [ "$mod" = "$a" ] && ok=1
  done
  if [ "$ok" -ne 1 ]; then
    echo "FAIL: dependency $mod is not on the license allowlist" >&2
    echo "      vet its license, then add it to scripts/check-deps.sh deliberately" >&2
    status=1
  fi
done < <(go list -m all 2>/dev/null | tail -n +2)

if [ "$status" -eq 0 ]; then
  echo "license allowlist: all module dependencies permitted"
fi
exit "$status"
