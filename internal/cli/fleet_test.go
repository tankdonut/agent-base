package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/fleet"
)

// runFleet executes the command tree and returns the exit code plus
// captured stdout (and the error text on failure) — check findings
// assert on the printed lines. Whitespace runs collapse to single
// spaces so table padding never breaks assertions.
func runFleet(t *testing.T, args ...string) (int, string) {
	t.Helper()
	root := NewRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	root.SilenceErrors = true
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		return 1, collapse(out.String()) + err.Error() + "\n"
	}
	return 0, collapse(out.String())
}

func collapse(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// makeFleetRepo scaffolds a fleet root with fleet.yaml and spec
// markers for the named agents; extra customizes the tree per test.
func makeFleetRepo(t *testing.T, manifest string, agents []string, extra func(root string)) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fleet.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range agents {
		dir := filepath.Join(root, "agents", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(`{"agent":{"name":"`+name+`"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		extra(root)
	}
	return root
}

func TestFleetLs(t *testing.T) {
	root := makeFleetRepo(t, `plane:
  enabled: true
  name: my-plane
services:
  browser-mcp: {}
agents:
  grow: {}
  trade:
    platform: fly
    fly: {app: trade-agent}
`, []string{"grow", "trade"}, nil)
	restore := chdir(t, root)
	defer restore()

	code, out := runFleet(t, "fleet", "ls")
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	for _, want := range []string{
		"grow compose 18789 shared agents/grow",
		"trade fly 18790 shared agents/trade",
		"svc:browser-mcp - - - services/browser-mcp",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q:\n%s", want, out)
		}
	}
}

func TestFleetLsLegacyRepoPointsAtMigrate(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent", "spec.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()

	code, out := runFleet(t, "fleet", "ls")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out, "agentctl migrate") {
		t.Errorf("out = %q, want migrate hint", out)
	}
}

func TestFleetCheckClean(t *testing.T) {
	root := makeFleetRepo(t, "agents:\n  grow: {}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	defer restore()

	code, out := runFleet(t, "fleet", "check")
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "1 agent(s), 0 service(s), plane off — 0 error(s), 0 warning(s)") {
		t.Errorf("summary missing: %s", out)
	}
}

func TestFleetCheckFindings(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		agents   []string
		extra    func(root string)
		wantCode int
		wantOut  []string
		notWant  string
	}{
		{
			name:     "missing agent marker fails",
			manifest: "agents: {grow: {}, ghost: {}}\n",
			agents:   []string{"grow"},
			wantCode: 1,
			wantOut:  []string{"FAIL: agents.ghost: agents/ghost/spec.json missing"},
		},
		{
			name:     "service without compose file fails",
			manifest: "services: {db: {}}\nagents: {grow: {}}\n",
			agents:   []string{"grow"},
			wantCode: 1,
			wantOut:  []string{"FAIL: services.db: compose.yml missing"},
		},
		{
			name:     "env port ownership is a hard error",
			manifest: "agents: {grow: {}}\n",
			agents:   []string{"grow"},
			extra: func(root string) {
				dir := filepath.Join(root, "agents", "grow")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ZAI_API_KEY=x\nAGENT_GATEWAY_PORT=18790\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: 1,
			wantOut:  []string{"FAIL: agents.grow: agent/.env sets AGENT_GATEWAY_PORT", "manifest owns gateway ports"},
		},
		{
			name:     "unknown platform fails",
			manifest: "agents: {grow: {platform: k8s}}\n",
			agents:   []string{"grow"},
			wantCode: 1,
			wantOut:  []string{"FAIL: agents.grow.platform: unknown platform \"k8s\""},
		},
		{
			name:     "orphan agent dir warns",
			manifest: "agents: {grow: {}}\n",
			agents:   []string{"grow"},
			extra: func(root string) {
				dir := filepath.Join(root, "agents", "drift")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: 0,
			wantOut:  []string{"WARN: agents/drift carries spec.json but is not registered"},
		},
		{
			name:     "retired agentctl config warns",
			manifest: "agents: {grow: {}}\n",
			agents:   []string{"grow"},
			extra: func(root string) {
				if err := os.WriteFile(filepath.Join(root, ".agentctl.yaml"), []byte("platform: compose\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: 0,
			wantOut:  []string{"WARN: .agentctl.yaml is retired"},
		},
		{
			name:     "plane advisory at two agents",
			manifest: "agents: {grow: {}, trade: {}}\n",
			agents:   []string{"grow", "trade"},
			wantCode: 0,
			wantOut:  []string{"WARN: 2 agents with plane disabled"},
		},
		{
			name:     "stale rendered artifact warns",
			manifest: "agents: {grow: {}}\n",
			agents:   []string{"grow"},
			extra: func(root string) {
				if err := os.MkdirAll(filepath.Join(root, "agents", "grow"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "agents", "grow", "compose.yml"), []byte("hand-edited: true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: 0,
			wantOut:  []string{"WARN: agents.grow: compose.yml is stale or hand-edited"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := makeFleetRepo(t, tt.manifest, tt.agents, tt.extra)
			restore := chdir(t, root)
			defer restore()

			code, out := runFleet(t, "fleet", "check")
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; out = %s", code, tt.wantCode, out)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("out missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestFleetCheckFreshArtifactNoDrift(t *testing.T) {
	root := makeFleetRepo(t, "agents: {grow: {}}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	defer restore()

	if code, out := runFleet(t, "fleet", "check"); code != 0 {
		t.Fatalf("pre-materialize check failed: %s", out)
	}
	// Materialize via the package API (the verbs write it in P1) and
	// confirm the drift warning stays silent on the fresh artifact.
	m, err := fleet.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.MaterializeAgentCompose(m, "grow"); err != nil {
		t.Fatal(err)
	}
	code, out := runFleet(t, "fleet", "check")
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if strings.Contains(out, "stale or hand-edited") {
		t.Errorf("fresh artifact flagged as drift:\n%s", out)
	}
}

func TestFleetCheckFromAgentSubdir(t *testing.T) {
	root := makeFleetRepo(t, "agents: {grow: {}}\n", []string{"grow"}, nil)
	restore := chdir(t, filepath.Join(root, "agents", "grow"))
	defer restore()

	code, out := runFleet(t, "fleet", "check")
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "0 error(s)") {
		t.Errorf("summary missing: %s", out)
	}
}
