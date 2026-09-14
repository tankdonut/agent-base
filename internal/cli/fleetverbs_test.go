package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/fleet"
)

// twoAgentRepo builds a fleet with grow healthy and trade broken (spec
// marker missing) so failure isolation is observable in one run.
func twoAgentRepo(t *testing.T) string {
	t.Helper()
	root := makeFleetRepo(t, "agents:\n  grow: {}\n  trade: {}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	t.Cleanup(restore)
	return root
}

func TestFleetScopeDemandsExplicitScope(t *testing.T) {
	twoAgentRepo(t)

	code, out := runFleet(t, "fleet", "deploy")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no implicit batch)", code)
	}
	if !strings.Contains(out, "never implicit") || !strings.Contains(out, "--agent") {
		t.Errorf("out = %q, want explicit-scope error", out)
	}
}

func TestFleetDeployFleetOfOneImplicit(t *testing.T) {
	root := makeFleetRepo(t, "agents:\n  grow: {}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	t.Cleanup(restore)
	stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "deploy")
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "1 agent(s): 1 ok, 0 failed") {
		t.Errorf("summary missing: %s", out)
	}
}

func TestFleetDeployUnknownAgent(t *testing.T) {
	twoAgentRepo(t)
	stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "deploy", "--agent", "ghost")
	if code != 1 || !strings.Contains(out, "not registered") || !strings.Contains(out, "grow") {
		t.Fatalf("code = %d out = %q, want unknown-agent listing known agents", code, out)
	}
}

func TestFleetDeployAgentAndAllMutuallyExclusive(t *testing.T) {
	twoAgentRepo(t)
	stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "deploy", "--agent", "grow", "--all")
	if code != 1 || !strings.Contains(out, "mutually exclusive") {
		t.Fatalf("code = %d out = %q", code, out)
	}
}

func TestFleetDeployAllIsolatesFailures(t *testing.T) {
	twoAgentRepo(t)
	r := stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "deploy", "--all")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 with one broken agent; out = %s", code, out)
	}
	for _, want := range []string{
		"==> grow",
		"==> trade",
		"FAIL trade:",
		"2 agent(s): 1 ok, 1 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q:\n%s", want, out)
		}
	}
	// The healthy agent still built + converged despite the sibling's
	// failure; the broken one never reached the engine.
	var sawBuild bool
	for _, call := range r.calls {
		if len(call) >= 5 && call[1] == "compose" && call[4] == "build" {
			sawBuild = true
		}
	}
	if !sawBuild {
		t.Errorf("no compose build recorded for the healthy agent: %v", r.calls)
	}
}

func TestFleetDeploySingleScopedAgent(t *testing.T) {
	twoAgentRepo(t)
	r := stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "deploy", "--agent", "grow")
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if strings.Contains(out, "==> trade") || strings.Contains(out, "FAIL trade") {
		t.Errorf("scoped deploy touched the sibling:\n%s", out)
	}
	if len(r.calls) == 0 {
		t.Error("no engine calls recorded")
	}
}

func TestFleetStopStartBackupStatusFanOut(t *testing.T) {
	root := twoAgentRepo(t)
	// Both agents healthy for the lifecycle verbs.
	if err := os.MkdirAll(filepath.Join(root, "agents", "trade"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := `{"specVersion": 1}`
	if err := os.WriteFile(filepath.Join(root, "agents", "trade", "spec.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "trade", "Dockerfile"), []byte("FROM ghcr.io/tankdonut/agent-base:2026.09.05\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "trade", ".env"), []byte("ZAI_API_KEY=k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubbedRunner(t, "podman")

	for _, verb := range []string{"stop", "start", "backup", "status"} {
		code, out := runFleet(t, "fleet", verb, "--all")
		if code != 0 {
			t.Fatalf("fleet %s --all failed:\n%s", verb, out)
		}
		if !strings.Contains(out, "2 agent(s): 2 ok, 0 failed") {
			t.Errorf("fleet %s summary wrong:\n%s", verb, out)
		}
	}
}

func TestFleetVerbsMaterializeEnvelopes(t *testing.T) {
	root := twoAgentRepo(t)
	// trade resolves too: the verb must materialize its envelope before
	// its (deliberately missing) Dockerfile fails the derive.
	if err := os.MkdirAll(filepath.Join(root, "agents", "trade"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "trade", "spec.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubbedRunner(t, "podman")

	if _, out := runFleet(t, "fleet", "status", "--all"); !strings.Contains(out, "2 agent(s):") {
		t.Fatalf("status did not fan out:\n%s", out)
	}
	for _, name := range []string{"grow", "trade"} {
		if _, err := os.Stat(filepath.Join(root, "agents", name, fleet.RenderedComposeName)); err != nil {
			t.Errorf("agents/%s envelope not materialized by the verb: %v", name, err)
		}
	}
}
