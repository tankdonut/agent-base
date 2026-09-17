//go:build integration

// Package integration hosts Tier A of the e2e re-imagining: engine
// integration tests that converge REAL compose stacks through the
// REAL platform adapter and the dir-scoped Runner. Budgeted and
// contained by internal/e2e — a wedge fails with artifacts, never a
// silent hang.
//
// Run: ./make.sh agentctl-integration (go test -tags=integration).
// Skips loudly when no engine (podman/docker) or when the pinned base
// image cannot be pulled.
package integration
