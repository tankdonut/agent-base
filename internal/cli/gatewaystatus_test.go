package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgentEnvToken appends a gateway token line to one agent's .env.
func writeAgentEnvToken(t *testing.T, root, name, token string) error {
	t.Helper()
	path := filepath.Join(root, "agents", name, ".env")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("OPENCLAW_GATEWAY_TOKEN=" + token + "\n")
	return err
}

func TestApprovalsScopeAndFlagContract(t *testing.T) {
	twoAgentRepo(t)
	stubbedRunner(t, "podman")

	t.Run("multi-agent roster demands explicit --agent", func(t *testing.T) {
		code, out := runFleet(t, "approvals")
		if code != 1 || !strings.Contains(out, "demand an explicit --agent") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})

	t.Run("unknown agent named", func(t *testing.T) {
		code, out := runFleet(t, "approvals", "--agent", "ghost")
		if code != 1 || !strings.Contains(out, "not registered") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})

	t.Run("id without a decision", func(t *testing.T) {
		code, out := runFleet(t, "approvals", "--agent", "grow", "--id", "ap_1")
		if code != 1 || !strings.Contains(out, "--approve or --deny") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})

	t.Run("both decisions refused", func(t *testing.T) {
		code, out := runFleet(t, "approvals", "--agent", "grow", "--id", "ap_1", "--approve", "--deny")
		if code != 1 || !strings.Contains(out, "mutually exclusive") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})

	t.Run("decision without id", func(t *testing.T) {
		code, out := runFleet(t, "approvals", "--agent", "grow", "--deny")
		if code != 1 || !strings.Contains(out, "need --id") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})

	t.Run("no token in agent env", func(t *testing.T) {
		code, out := runFleet(t, "approvals", "--agent", "grow")
		if code != 1 || !strings.Contains(out, "OPENCLAW_GATEWAY_TOKEN") {
			t.Fatalf("code = %d out = %q", code, out)
		}
	})
}

func TestFleetStatusLiveDegradesPerAgent(t *testing.T) {
	root := twoAgentRepo(t)
	stubbedRunner(t, "podman")

	// grow carries a token but nothing listens on its port — the row
	// FAILs (dial refused, fast) without taking the run down; trade
	// has no token at all. Any failure exits 1 after the full table.
	if err := writeAgentEnvToken(t, root, "grow", "sk-live-canary"); err != nil {
		t.Fatal(err)
	}
	code, out := runFleet(t, "fleet", "status", "--live", "--all")
	if code != 1 {
		t.Fatalf("all-dead probes must exit 1, got %d:\n%s", code, out)
	}
	for _, want := range []string{"AGENT", "GATEWAY", "grow", "trade", "FAIL"} {
		if !strings.Contains(out, want) {
			t.Errorf("live table missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-live-canary") {
		t.Errorf("token material leaked into output:\n%s", out)
	}
}
