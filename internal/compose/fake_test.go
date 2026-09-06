package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records every Run call (full argv) and resolves LookPath
// from a fixed set of binary names. failArgv makes specific calls fail.
type fakeRunner struct {
	calls    [][]string
	envs     [][]string
	look     map[string]bool
	failArgv [][]string
}

func newFakeRunner(look ...string) *fakeRunner {
	r := &fakeRunner{look: map[string]bool{}}
	for _, n := range look {
		r.look[n] = true
	}
	return r
}

func (f *fakeRunner) Run(env []string, name string, args ...string) error {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	f.envs = append(f.envs, env)
	for _, bad := range f.failArgv {
		if fmt.Sprint(call) == fmt.Sprint(bad) {
			return fmt.Errorf("fake failure: %v", call)
		}
	}
	return nil
}

func (f *fakeRunner) RunOutput(env []string, name string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("fake: output capture not configured")
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.look[name] {
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("%s: not found", name)
}

func assertCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// writeProject materializes a fixture project tree in a temp dir.
func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const fixtureDockerfile = `# project image
FROM ghcr.io/tankdonut/agent-base:2026.08.28
COPY agent/spec.json /opt/agent/spec.json
`

const fixtureSpec = `{
  "specVersion": 1,
  "setup": {"auth_choice": "zai-coding-global"},
  "model": {"fallback": "{env:FALLBACK_MODEL}"},
  "config": [
    {"path": "channels.telegram.allowFrom", "value": "{env:TELEGRAM_ALLOWED_USERS}", "if_env": ["TELEGRAM_ALLOWED_USERS"]}
  ],
  "mcp_servers": [
    {"url": "https://example.test/mcp?key={env:PROVIDER_KEY}"}
  ]
}`

// TestSecretsCanary locks the secrets-handling contract the container
// image enforces via its own canaries: a value planted in agent/.env
// must never reach child argv or injected environments — compose reads
// the env file itself, and validate substitutes dummies.
func TestSecretsCanary(t *testing.T) {
	const canary = "CANARY-7f3a9d1c-value"
	root := writeProject(t, map[string]string{
		"agent/spec.json":    fixtureSpec,
		"agent/Dockerfile":   fixtureDockerfile,
		"agent/.env.example": "#FALLBACK_MODEL=\n",
		"agent/.env":         "FALLBACK_MODEL=m\nPROVIDER_KEY=" + canary + "\nTELEGRAM_ALLOWED_USERS=" + canary + "\nZAI_API_KEY=" + canary + "\nOPENCLAW_GATEWAY_TOKEN=" + canary + "\n",
	})
	r := newFakeRunner("podman", "git")
	for _, fn := range []func() error{
		func() error { return Up(r, "podman", root) },
		func() error { return Dev(r, "podman", root) },
		func() error { return Destroy(r, "podman", false) },
		func() error { return Validate(r, "podman", root) },
	} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	for i, call := range r.calls {
		for _, arg := range call {
			if strings.Contains(arg, canary) {
				t.Fatalf("call %d argv leaks the agent/.env canary: %v", i, call)
			}
		}
		for _, kv := range r.envs[i] {
			if strings.Contains(kv, canary) {
				t.Fatalf("call %d environment leaks the agent/.env canary: %s", i, kv)
			}
		}
	}
}
