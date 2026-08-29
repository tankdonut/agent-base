package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeRunner records every Run call (full argv) and resolves LookPath
// from a fixed set of binary names. failArgv makes specific calls fail,
// e.g. a missing git ref for the worktree branch-existence probe.
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

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.look[name] {
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("%s: not found", name)
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
