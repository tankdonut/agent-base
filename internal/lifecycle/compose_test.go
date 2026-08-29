package lifecycle

import (
	"strings"
	"testing"
)

func TestLifecycleArgv(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json": fixtureSpec,
		"agent/.env":      "ZAI_API_KEY=x\n",
	})
	newRunner := func() *fakeRunner { return newFakeRunner("podman", "docker") }

	tests := []struct {
		name      string
		run       func(r Runner) error
		wantCalls [][]string
	}{
		{"up", func(r Runner) error { return Up(r, "podman", root) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "up", "-d"},
		}},
		{"dev", func(r Runner) error { return Dev(r, "podman", root) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "-f", "compose.dev.yml", "up", "-d"},
		}},
		{"down", func(r Runner) error { return Down(r, "docker") }, [][]string{
			{"docker", "compose", "-f", "compose.yml", "down"},
		}},
		{"logs passthrough", func(r Runner) error { return Logs(r, "podman", []string{"-f", "agent"}) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "logs", "-f", "agent"},
		}},
		{"logs bare", func(r Runner) error { return Logs(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "logs"},
		}},
		{"mcp login passthrough", func(r Runner) error {
			return Mcp(r, "podman", []string{"login", "docs", "--code", "abc123"})
		}, [][]string{
			{"podman", "compose", "-f", "compose.yml", "exec", "agent", "openclaw", "mcp", "login", "docs", "--code", "abc123"},
		}},
		{"mcp bare", func(r Runner) error { return Mcp(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "exec", "agent", "openclaw", "mcp"},
		}},
		{"build-images", func(r Runner) error { return BuildImages(r, "podman") }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build"},
		}},
		{"restart one service", func(r Runner) error { return Restart(r, "podman", []string{"agent"}) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "restart", "agent"},
		}},
		{"restart all", func(r Runner) error { return Restart(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "restart"},
		}},
		{"rebuild services", func(r Runner) error { return Rebuild(r, "podman", []string{"agent"}) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build", "agent"},
			{"podman", "compose", "-f", "compose.yml", "up", "-d", "--force-recreate", "agent"},
		}},
		{"rebuild all", func(r Runner) error { return Rebuild(r, "podman", nil) }, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build"},
			{"podman", "compose", "-f", "compose.yml", "up", "-d", "--force-recreate"},
		}},
		{"update pulls then rebuilds", func(r Runner) error { return Update(r, "podman", root) }, [][]string{
			{"git", "pull", "--ff-only"},
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
			assertCalls(t, r, tt.wantCalls)
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

func TestUpdateGatesOnEnvFile(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": fixtureSpec}) // no agent/.env
	r := newFakeRunner("podman")
	if err := Update(r, "podman", root); err == nil || !strings.Contains(err.Error(), "secrets init") {
		t.Fatalf("Update without agent/.env: err = %v, want gate error mentioning secrets init", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("gate failure must not exec anything, got %v", r.calls)
	}
}

func TestUpdatePullFailureStopsRebuild(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": fixtureSpec, "agent/.env": "ZAI_API_KEY=x\n"})
	r := newFakeRunner("podman")
	r.failArgv = [][]string{{"git", "pull", "--ff-only"}}
	if err := Update(r, "podman", root); err == nil || !strings.Contains(err.Error(), "git pull") {
		t.Fatalf("Update with failing pull: err = %v, want git pull error", err)
	}
	if len(r.calls) != 1 {
		t.Errorf("rebuild must not run after a failed pull, calls = %v", r.calls)
	}
}

func TestNilRunnerNeverPanics(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": fixtureSpec, "agent/.env": "X=1\n"})
	funcs := map[string]func() error{
		"up":       func() error { return Up(nil, "podman", root) },
		"down":     func() error { return Down(nil, "podman") },
		"logs":     func() error { return Logs(nil, "podman", nil) },
		"validate": func() error { return Validate(nil, "podman", root) },
		"check":    func() error { return PreCommitCheck(nil) },
		"hooks":    func() error { return PreCommitHooks(nil) },
		"worktree": func() error { return WorktreeCreate(nil, root, "b") },
		"open":     func() error { return Open(nil, root, 18789, &strings.Builder{}) },
		"mcp":      func() error { return Mcp(nil, "podman", nil) },
		"edit":     func() error { return SecretsEdit(nil, "vi", "/tmp/x") },
		"update":   func() error { return Update(nil, "podman", root) },
	}
	for name, fn := range funcs {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil || !strings.Contains(err.Error(), "nil runner") {
				t.Fatalf("%s with nil runner: err = %v, want nil-runner error", name, err)
			}
		})
	}
}

func assertCalls(t *testing.T, r *fakeRunner, want [][]string) {
	t.Helper()
	if len(r.calls) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot:  %v\nwant: %v", len(r.calls), len(want), r.calls, want)
	}
	for i := range want {
		if strings.Join(r.calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, r.calls[i], want[i])
		}
	}
}
