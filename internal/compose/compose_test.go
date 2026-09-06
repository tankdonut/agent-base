package compose

import (
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/process"
)

func TestLifecycleArgv(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json": fixtureSpec,
		"agent/.env":      "ZAI_API_KEY=x\n",
	})
	newRunner := func() *fakeRunner { return newFakeRunner("podman", "docker") }

	tests := []struct {
		name      string
		run       func(r process.Runner) error
		wantCalls [][]string
	}{
		{"up", func(r process.Runner) error { return Up(r, "podman", root) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "up", "-d"},
		}},
		{"dev", func(r process.Runner) error { return Dev(r, "podman", root) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "-f", "compose.dev.yml", "up", "-d"},
		}},
		{"down", func(r process.Runner) error { return Down(r, "docker") }, [][]string{
			{"docker", "compose", "-f", "compose.yml", "down"},
		}},
		{"stop", func(r process.Runner) error { return Stop(r, "podman") }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "stop"},
		}},
		{"start", func(r process.Runner) error { return Start(r, "podman") }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "start"},
		}},
		{"destroy keeps volumes", func(r process.Runner) error { return Destroy(r, "podman", false) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "down"},
		}},
		{"destroy with volumes", func(r process.Runner) error { return Destroy(r, "podman", true) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "down", "-v"},
		}},
		{"ps", func(r process.Runner) error { return Ps(r, "podman") }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "ps"},
		}},
		{"logs passthrough", func(r process.Runner) error { return Logs(r, "podman", []string{"-f", "agent"}) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "logs", "-f", "agent"},
		}},
		{"logs bare", func(r process.Runner) error { return Logs(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "logs"},
		}},
		{"mcp login passthrough", func(r process.Runner) error {
			return Mcp(r, "podman", []string{"login", "docs", "--code", "abc123"})
		}, [][]string{
			{"podman", "compose", "-f", "compose.yml", "exec", "agent", "openclaw", "mcp", "login", "docs", "--code", "abc123"},
		}},
		{"mcp bare", func(r process.Runner) error { return Mcp(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "exec", "agent", "openclaw", "mcp"},
		}},
		{"build-images", func(r process.Runner) error { return BuildImages(r, "podman") }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build"},
		}},
		{"restart one service", func(r process.Runner) error { return Restart(r, "podman", []string{"agent"}) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "restart", "agent"},
		}},
		{"restart all", func(r process.Runner) error { return Restart(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "restart"},
		}},
		{"rebuild services", func(r process.Runner) error { return Rebuild(r, "podman", []string{"agent"}) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build", "agent"},
			{"podman", "compose", "-f", "compose.yml", "up", "-d", "--force-recreate", "agent"},
		}},
		{"rebuild all", func(r process.Runner) error { return Rebuild(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build"},
			{"podman", "compose", "-f", "compose.yml", "up", "-d", "--force-recreate"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRunner()
			if err := tt.run(r); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertCalls(t, r.calls, tt.wantCalls)
		})
	}
}

func TestUpGatesOnEnvFile(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": fixtureSpec}) // no agent/.env
	r := newFakeRunner("podman")
	if err := Up(r, "podman", root); err == nil || !strings.Contains(err.Error(), "secrets init") {
		t.Fatalf("Up without agent/.env: err = %v, want gate error mentioning secrets init", err)
	}
	if err := Dev(r, "podman", root); err == nil || !strings.Contains(err.Error(), "secrets init") {
		t.Fatalf("Dev without agent/.env: err = %v, want gate error mentioning secrets init", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("gate failure must not exec anything, got %v", r.calls)
	}
}

func TestNilRunnerNeverPanics(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": fixtureSpec, "agent/.env": "X=1\n"})
	funcs := map[string]func() error{
		"up":       func() error { return Up(nil, "podman", root) },
		"down":     func() error { return Down(nil, "podman") },
		"logs":     func() error { return Logs(nil, "podman", nil) },
		"validate": func() error { return Validate(nil, "podman", root) },
		"mcp":      func() error { return Mcp(nil, "podman", nil) },
		"stop":     func() error { return Stop(nil, "podman") },
		"start":    func() error { return Start(nil, "podman") },
		"destroy":  func() error { return Destroy(nil, "podman", false) },
	}
	for name, fn := range funcs {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil || !strings.Contains(err.Error(), "nil runner") {
				t.Fatalf("%s with nil runner: err = %v, want nil-runner error", name, err)
			}
		})
	}
}
