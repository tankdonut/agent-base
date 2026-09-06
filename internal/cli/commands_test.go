package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/process"
)

// stubRunner records Run calls and resolves LookPath from a fixed set —
// the cli-level twin of the foundation packages' fakes, so command
// wiring tests run hermetically (no docker/podman on the host).
type stubRunner struct {
	calls    [][]string
	look     map[string]bool
	failArgv [][]string
}

func newStubRunner(look ...string) *stubRunner {
	r := &stubRunner{look: map[string]bool{}}
	for _, n := range look {
		r.look[n] = true
	}
	return r
}

func (s *stubRunner) Run(env []string, name string, args ...string) error {
	call := append([]string{name}, args...)
	s.calls = append(s.calls, call)
	for _, bad := range s.failArgv {
		if strings.Join(call, " ") == strings.Join(bad, " ") {
			return fmt.Errorf("fake failure: %v", call)
		}
	}
	return nil
}

func (s *stubRunner) LookPath(name string) (string, error) {
	if s.look[name] {
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

// fixtureProject materializes a minimal but contract-shaped project:
// spec with one required env ref, pinned Dockerfile, complete .env, and
// a compose.yml with the agent service and both contract volumes.
func fixtureProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"agent/spec.json": `{
  "specVersion": 1,
  "setup": {"auth_choice": "zai-coding-global"},
  "model": {"fallback": "{env:FALLBACK_MODEL}"}
}`,
		"agent/Dockerfile": "FROM ghcr.io/tankdonut/agent-base:2026.09.05\nCOPY agent/spec.json /opt/agent/spec.json\n",
		"agent/.env":       "FALLBACK_MODEL=m\nZAI_API_KEY=k\n",
		"compose.yml": `name: fixture-agent
services:
  agent:
    build: {context: ., dockerfile: agent/Dockerfile}
volumes:
  agent-data:
  agent-backups:
`,
	}
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

// stubbedRunner swaps the cli runner seam for a stub and restores it.
func stubbedRunner(t *testing.T, look ...string) *stubRunner {
	t.Helper()
	r := newStubRunner(look...)
	old := newRunner
	newRunner = func() process.Runner { return r }
	t.Cleanup(func() { newRunner = old })
	return r
}

func execIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	restore := chdir(t, dir)
	defer restore()
	root := NewRootCommand()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestDoctorPassesOnContractProject(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, want := range []string{"spec.json parses", "base image pinned", "required vars", "manifest lints", "all checks passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorFailsOnIncompleteProject(t *testing.T) {
	root := fixtureProject(t)
	if err := os.Remove(filepath.Join(root, "agent", ".env")); err != nil {
		t.Fatal(err)
	}
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor")
	if err == nil {
		t.Fatal("doctor must fail without agent/.env")
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("output lacks FAIL lines:\n%s", out)
	}
}

func TestDeployDryRunChecksOnly(t *testing.T) {
	root := fixtureProject(t)
	r := stubbedRunner(t, "podman")
	out, err := execIn(t, root, "deploy", "--dry-run")
	if err != nil {
		t.Fatalf("deploy --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry run complete") {
		t.Errorf("output = %q", out)
	}
	if len(r.calls) != 0 {
		t.Errorf("dry run must not exec anything, got %v", r.calls)
	}
}

func TestDeployBuildsThenUps(t *testing.T) {
	root := fixtureProject(t)
	r := stubbedRunner(t, "podman")
	out, err := execIn(t, root, "deploy")
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	want := [][]string{
		{"podman", "compose", "-f", "compose.yml", "build"},
		{"podman", "compose", "-f", "compose.yml", "up", "-d"},
	}
	if len(r.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
	for i := range want {
		if strings.Join(r.calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, r.calls[i], want[i])
		}
	}
}

func TestDeployRefusesWithoutEnvFile(t *testing.T) {
	root := fixtureProject(t)
	if err := os.Remove(filepath.Join(root, "agent", ".env")); err != nil {
		t.Fatal(err)
	}
	r := stubbedRunner(t, "podman")
	_, err := execIn(t, root, "deploy")
	if err == nil || !strings.Contains(err.Error(), "secrets init") {
		t.Fatalf("err = %v, want secrets gate", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("gate failure must not exec anything, got %v", r.calls)
	}
}

func TestDestroyVolumeConfirmations(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")

	out, err := execIn(t, root, "destroy", "--volumes", "--yes")
	if err != nil {
		t.Fatalf("destroy --volumes --yes: %v\n%s", err, out)
	}
	if !strings.Contains(out, "INCLUDING volumes") {
		t.Errorf("output = %q", out)
	}

	out, err = execIn(t, root, "destroy")
	if err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "volumes kept") {
		t.Errorf("output = %q", out)
	}
}

func TestConfirmDestroyRequiresProjectName(t *testing.T) {
	ok := strings.NewReader("fixture-agent\n")
	if err := confirmDestroy(ok, &bytes.Buffer{}, "fixture-agent"); err != nil {
		t.Fatalf("matching name: %v", err)
	}
	wrong := strings.NewReader("nope\n")
	if err := confirmDestroy(wrong, &bytes.Buffer{}, "fixture-agent"); err == nil {
		t.Fatal("mismatched name must abort")
	}
}

func TestPlatformLsMarksPinned(t *testing.T) {
	root := fixtureProject(t)
	if err := os.WriteFile(filepath.Join(root, ConfigName), []byte("platform: compose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "platform", "ls")
	if err != nil {
		t.Fatalf("platform ls: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pinned  compose") {
		t.Errorf("output lacks pinned marker:\n%s", out)
	}
}

func TestPlatformSetUnknownFailsClosed(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	_, err := execIn(t, root, "platform", "set", "fly")
	if err == nil || !strings.Contains(err.Error(), "unknown platform") {
		t.Fatalf("err = %v, want unknown platform", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ConfigName)); !os.IsNotExist(statErr) {
		t.Error("failed set must not write .agentctl.yaml")
	}
}

func TestPlatformSetPinsAndChecks(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "platform", "set", "compose")
	if err != nil {
		t.Fatalf("platform set compose: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(root, ConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "platform: compose") {
		t.Errorf("config = %q", data)
	}
}

func TestPlatformCheckPasses(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "platform", "check")
	if err != nil {
		t.Fatalf("platform check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "check passed") {
		t.Errorf("output = %q", out)
	}
}

func TestPlatformCheckFailsOnContractlessManifest(t *testing.T) {
	root := fixtureProject(t)
	broken := "services:\n  agent:\n    build: {context: .}\n"
	if err := os.WriteFile(filepath.Join(root, "compose.yml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	stubbedRunner(t, "podman")
	_, err := execIn(t, root, "platform", "check")
	if err == nil || !strings.Contains(err.Error(), "agent-data") {
		t.Fatalf("err = %v, want missing volume error", err)
	}
}

func TestReleaseVerbsNeedProject(t *testing.T) {
	stubbedRunner(t, "podman")
	if _, err := execIn(t, t.TempDir(), "status"); err == nil {
		t.Fatal("status outside a project must fail")
	}
}
