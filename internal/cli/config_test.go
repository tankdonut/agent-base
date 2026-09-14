package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/fleet"
)

func TestLoadConfigDefaults(t *testing.T) {
	restore := chdir(t, t.TempDir())
	defer restore()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Platform != "compose" {
		t.Errorf("platform = %q, want compose default", cfg.Platform)
	}
	if got := cfg.ComposeEnginePref(); got != "" {
		t.Errorf("engine pref = %q, want empty (auto)", got)
	}
	if got := cfg.ComposeGatewayPort(); got != 18789 {
		t.Errorf("gateway port = %d, want 18789 default", got)
	}
}

func TestLoadConfigFile(t *testing.T) {
	dir := t.TempDir()
	file := "platform: compose\ncompose:\n  engine: podman\n  gateway_port: 19000\n"
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Platform != "compose" {
		t.Errorf("platform = %q, want compose", cfg.Platform)
	}
	if got := cfg.ComposeEnginePref(); got != "podman" {
		t.Errorf("engine pref = %q, want podman", got)
	}
	if got := cfg.ComposeGatewayPort(); got != 19000 {
		t.Errorf("gateway port = %d, want 19000", got)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	file := "compose:\n  engine: podman\n"
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()

	t.Setenv("AGENTCTL_COMPOSE_ENGINE", "docker")
	t.Setenv("AGENTCTL_COMPOSE_GATEWAY_PORT", "19100")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ComposeEnginePref(); got != "docker" {
		t.Errorf("engine pref = %q, want docker from env", got)
	}
	if got := cfg.ComposeGatewayPort(); got != 19100 {
		t.Errorf("gateway port = %d, want 19100 from env", got)
	}
}

func TestLoadConfigUnknownKeysFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{"legacy engine names its rename", "engine: docker\n", "compose.engine"},
		{"legacy gateway_port names its rename", "gateway_port: 19000\n", "compose.gateway_port"},
		{"arbitrary unknown key", "turbo: true\n", "unknown key"},
		{"platform must be a string", "platform: 7\n", "want a string"},
		{"compose must be a mapping", "compose: hi\n", "want a mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte(tt.file), 0o644); err != nil {
				t.Fatal(err)
			}
			restore := chdir(t, dir)
			defer restore()
			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigCwdWinsOverProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent", "spec.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ConfigName), []byte("compose:\n  engine: podman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ConfigName), []byte("compose:\n  engine: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, nested)
	defer restore()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ComposeEnginePref(); got != "docker" {
		t.Errorf("engine pref = %q, want docker — cwd config must win over project root", got)
	}
}

func TestLoadConfigReadsProjectRootFromSubdir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agent", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent", "spec.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ConfigName), []byte("compose:\n  engine: podman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, filepath.Join(root, "agent", "nested"))
	defer restore()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ComposeEnginePref(); got != "podman" {
		t.Errorf("engine pref = %q, want podman from project-root config", got)
	}
}

func TestBadGatewayPortEnvFails(t *testing.T) {
	restore := chdir(t, t.TempDir())
	defer restore()
	t.Setenv("AGENTCTL_COMPOSE_GATEWAY_PORT", "not-a-port")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "AGENTCTL_COMPOSE_GATEWAY_PORT") {
		t.Fatalf("err = %v, want gateway port env error", err)
	}
}

func TestPinFleetAgentPlatform(t *testing.T) {
	newManifest := func(t *testing.T) *fleet.Manifest {
		t.Helper()
		body := "# fleet manifest\nplane:\n  gateway_base_port: 18789\nagents:\n  grow:\n    gateway_port: 18789\n"
		root := t.TempDir()
		path := filepath.Join(root, fleet.ManifestName)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := fleet.LoadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("inserts the pin as the entry's first key", func(t *testing.T) {
		m := newManifest(t)
		if err := pinFleetAgentPlatform(m, "grow", "fly"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(m.Path)
		if err != nil {
			t.Fatal(err)
		}
		got := string(data)
		if !strings.Contains(got, "  grow:\n    platform: fly\n    gateway_port: 18789\n") {
			t.Errorf("pin misplaced:\n%s", got)
		}
		if !strings.Contains(got, "# fleet manifest") {
			t.Errorf("comments must survive:\n%s", got)
		}
	})

	t.Run("replaces an existing pin in place", func(t *testing.T) {
		m := newManifest(t)
		if err := pinFleetAgentPlatform(m, "grow", "fly"); err != nil {
			t.Fatal(err)
		}
		if err := pinFleetAgentPlatform(m, "grow", "compose"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(m.Path)
		if err != nil {
			t.Fatal(err)
		}
		got := string(data)
		if strings.Count(got, "platform:") != 1 || !strings.Contains(got, "platform: compose") {
			t.Errorf("pin not replaced exactly once:\n%s", got)
		}
	})

	t.Run("unknown agent fails closed", func(t *testing.T) {
		m := newManifest(t)
		err := pinFleetAgentPlatform(m, "ghost", "fly")
		if err == nil || !strings.Contains(err.Error(), "agents.ghost entry not found") {
			t.Fatalf("err = %v, want missing-entry error", err)
		}
	})
}

func TestLoadConfigFlyNamespace(t *testing.T) {
	dir := t.TempDir()
	file := "platform: fly\nfly:\n  app: my-agent\n  region: sjc\n"
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Platform != "fly" {
		t.Errorf("platform = %q", cfg.Platform)
	}
	if got := cfg.Namespaces["fly"]["app"]; got != "my-agent" {
		t.Errorf("fly.app = %v", got)
	}
}

func TestLoadConfigUnknownNamespaceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte("k8s: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v, want unknown namespace key error", err)
	}
}
