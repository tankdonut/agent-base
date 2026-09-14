package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/fleet"
)

func TestFleetAddScaffoldsAndRegisters(t *testing.T) {
	root := makeFleetRepo(t, "agents:\n  grow: {}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	t.Cleanup(restore)

	code, out := runFleet(t, "fleet", "add", "trade")
	if code != 0 {
		t.Fatalf("fleet add failed:\n%s", out)
	}
	for _, want := range []string{
		"wrote agents/trade/spec.json",
		"added trade (gateway port 18790)",
		"note: 2 agents with plane disabled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "trade", "spec.json")); err != nil {
		t.Errorf("scaffolded spec missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "README.md")); err == nil {
		// Repo files belong to init; add must not emit them over an
		// existing repo. README exists only when init wrote it — here it
		// must NOT have been created by add.
		t.Error("fleet add emitted a repo file (README.md) into a non-init repo")
	}

	// The registration loads back with the recorded port.
	m, err := fleet.LoadManifest(root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Agents["trade"].GatewayPort; got != 18790 {
		t.Errorf("trade port = %d, want 18790 (next free after grow)", got)
	}
	if !m.Agents["trade"].ExplicitPort {
		t.Error("fleet add must record an explicit port (additions never shift existing allocations)")
	}
}

func TestFleetAddExplicitPort(t *testing.T) {
	root := makeFleetRepo(t, "agents:\n  grow: {}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	t.Cleanup(restore)

	if code, out := runFleet(t, "fleet", "add", "trade", "--port", "19500"); code != 0 {
		t.Fatalf("fleet add --port failed:\n%s", out)
	} else if !strings.Contains(out, "gateway port 19500") {
		t.Errorf("explicit port not recorded:\n%s", out)
	}
}

func TestFleetAddRefusals(t *testing.T) {
	root := makeFleetRepo(t, "agents:\n  grow: {}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	t.Cleanup(restore)

	t.Run("already registered", func(t *testing.T) {
		if code, out := runFleet(t, "fleet", "add", "grow"); code != 1 || !strings.Contains(out, "already registered") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})
	t.Run("existing on-disk agent refuses overwrite", func(t *testing.T) {
		dir := filepath.Join(root, "agents", "drift")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, out := runFleet(t, "fleet", "add", "drift"); code != 1 || !strings.Contains(out, "refusing to overwrite") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})
	t.Run("invalid name", func(t *testing.T) {
		if code, out := runFleet(t, "fleet", "add", "BadName"); code != 1 || !strings.Contains(out, "valid directory/compose name") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})
}

func TestFleetAddWarnsOnImplicitPortShift(t *testing.T) {
	// zebra holds an implicit port (the only agent → base). Adding
	// alpha sorts in front of it and would shift zebra's allocation.
	root := makeFleetRepo(t, "agents:\n  zebra: {}\n", []string{"zebra"}, nil)
	restore := chdir(t, root)
	t.Cleanup(restore)

	code, out := runFleet(t, "fleet", "add", "alpha")
	if code != 0 {
		t.Fatalf("fleet add failed:\n%s", out)
	}
	if !strings.Contains(out, "agents.zebra holds an implicit port") {
		t.Errorf("shift warning missing:\n%s", out)
	}
}
