package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/platform/fly"
	"github.com/tankdonut/agent-base/internal/process"
)

// stubRunner records Run calls and resolves LookPath from a fixed set —
// the cli-level twin of the foundation packages' fakes, so command
// wiring tests run hermetically (no docker/podman on the host).
type stubRunner struct {
	calls       [][]string
	look        map[string]bool
	failArgv    [][]string
	runOutputOK bool
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

func (s *stubRunner) RunOutput(env []string, name string, args ...string) ([]byte, error) {
	if s.runOutputOK {
		return nil, nil
	}
	return nil, fmt.Errorf("fake: output capture not configured")
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
		"agent/Dockerfile":          "FROM ghcr.io/tankdonut/agent-base:2026.09.05\nCOPY agent/spec.json /opt/agent/spec.json\n",
		"agent/.env.example":        "#FALLBACK_MODEL=\n",
		"agent/.env":                "FALLBACK_MODEL=m\nZAI_API_KEY=k\n",
		"agent/automations/jobs.md": "---\nname: probe\ncron: 0 9 * * *\ndeliver: announce\n---\nbody\n",
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

func litellmFixture(t *testing.T) string {
	t.Helper()
	root := fixtureProject(t)
	if err := os.WriteFile(filepath.Join(root, "agent", "Dockerfile"),
		[]byte("FROM ghcr.io/tankdonut/agent-base:2026.09.12\nCOPY agent/spec.json /opt/agent/spec.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(root, "agent", "spec.json")
	body, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	flipped := strings.Replace(string(body), `"zai-coding-global"`, `"litellm-api-key"`, 1)
	if err := os.WriteFile(spec, []byte(flipped), 0o644); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(root, "agent", ".env")
	body, err = os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte(string(body)+"LITELLM_API_KEY=sk-doctor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "litellm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "litellm", ".env.example"), []byte("#LITELLM_MASTER_KEY=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "litellm", ".env"), []byte("LITELLM_MASTER_KEY=sk-doctor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// addLitellmSidecar rewrites the fixture compose.yml to carry the
// litellm service and model-net — the full blessed shape.
func addLitellmSidecar(t *testing.T, root string) {
	t.Helper()
	compose := filepath.Join(root, "compose.yml")
	body, err := os.ReadFile(compose)
	if err != nil {
		t.Fatal(err)
	}
	withSidecar := strings.Replace(string(body), "volumes:\n  agent-data:",
		`  litellm:
    image: ghcr.io/berriai/litellm:v1.100.0
networks:
  model-net: {}
volumes:
  agent-data:`, 1)
	if err := os.WriteFile(compose, []byte(withSidecar), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorWarnsWhenSidecarNotAdopted(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, want := range []string{
		`warn  provider "zai-coding-global" — litellm sidecar not adopted`,
		"Migrating an existing agent to LiteLLM",
		"all checks passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorLitellmShapeFailsOnMissingSidecar(t *testing.T) {
	root := litellmFixture(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor")
	if err == nil {
		t.Fatal("doctor must fail when the litellm spec lacks the compose sidecar")
	}
	if !strings.Contains(out, "compose.yml lacks the litellm sidecar") {
		t.Errorf("output lacks the shape FAIL line:\n%s", out)
	}
}

func TestDoctorLitellmShapePasses(t *testing.T) {
	root := litellmFixture(t)
	addLitellmSidecar(t, root)
	r := stubbedRunner(t, "podman")
	r.runOutputOK = true
	out, err := execIn(t, root, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, want := range []string{
		"litellm sidecar shape present (tree, compose service, model-net)",
		"real-image spec gate passed via podman",
		"all checks passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not adopted") {
		t.Errorf("litellm project must not carry the migration warn:\n%s", out)
	}
}

func TestDoctorFailsWhenImagePreDatesLitellmSeed(t *testing.T) {
	root := litellmFixture(t)
	addLitellmSidecar(t, root)
	if err := os.WriteFile(filepath.Join(root, "agent", "Dockerfile"),
		[]byte("FROM ghcr.io/tankdonut/agent-base:2026.08.31\nCOPY agent/spec.json /opt/agent/spec.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor")
	if err == nil {
		t.Fatal("doctor must fail when a litellm project pins a pre-litellm image")
	}
	for _, want := range []string{
		"pinned base image 2026.08.31 predates the litellm seed (2026.09.12)",
		"litellm sidecar shape present",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorSkipsGateWithoutEngine(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t)
	out, err := execIn(t, root, "doctor")
	if err == nil {
		t.Fatal("engineless host must fail on the platform check (pre-existing semantics)")
	}
	for _, want := range []string{
		`FAIL  platform "compose": no container engine found`,
		"warn  no compose engine — skipped the real-image spec gate",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorSkipsGateWithoutPinnedImage(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor")
	if err != nil {
		t.Fatalf("imageless host must warn, not fail: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not local — skipped the real-image spec gate") {
		t.Errorf("output lacks the image warn:\n%s", out)
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
	_, err := execIn(t, root, "platform", "set", "wat")
	if err == nil || !strings.Contains(err.Error(), "unknown platform") {
		t.Fatalf("err = %v, want unknown platform", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ConfigName)); !os.IsNotExist(statErr) {
		t.Error("failed set must not write .agentctl.yaml")
	}
}

func TestPlatformSetFlyScaffoldsAndPins(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman", "fly")
	out, err := execIn(t, root, "platform", "set", "fly", "--app", "my-agent", "--region", "sjc")
	if err != nil {
		t.Fatalf("platform set fly: %v\n%s", err, out)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "deploy", "fly.toml"))
	if err != nil {
		t.Fatalf("deploy/fly.toml not scaffolded: %v", err)
	}
	for _, want := range []string{
		`app = "my-agent"`,
		`primary_region = "sjc"`,
		`kill_signal = "SIGTERM"`,
		"kill_timeout = 300",
		`AGENT_SHUTDOWN_GRACE = "300"`,
		`destination = "/home/node/.openclaw"`,
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("fly.toml lacks %q:\n%s", want, manifest)
		}
	}
	cfgData, err := os.ReadFile(filepath.Join(root, ConfigName))
	if err != nil || !strings.Contains(string(cfgData), "platform: fly") {
		t.Errorf("platform not pinned: %v %s", err, cfgData)
	}
	if !strings.Contains(out, "fly launch") {
		t.Errorf("output lacks fly next steps:\n%s", out)
	}

	// Re-running set fly must refuse to clobber the manifest.
	out, err = execIn(t, root, "platform", "set", "fly", "--app", "other", "--region", "iad")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("re-scaffold must refuse to overwrite: %v", err)
	}
}

func TestPlatformSetFlyRequiresFlags(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman", "fly")
	_, err := execIn(t, root, "platform", "set", "fly")
	if err == nil || !strings.Contains(err.Error(), "--app") {
		t.Fatalf("err = %v, want --app requirement", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "deploy", "fly.toml")); !os.IsNotExist(statErr) {
		t.Error("failed set must not scaffold")
	}
}

func TestDestroyGateOnNonVolumePreservingPlatform(t *testing.T) {
	root := fixtureProject(t)
	if err := os.WriteFile(filepath.Join(root, ConfigName), []byte("platform: fly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubbedRunner(t, "podman", "fly")
	if err := fly.ScaffoldConfig(root, "my-agent", "sjc"); err != nil {
		t.Fatal(err)
	}
	_, err := execIn(t, root, "destroy")
	if err == nil || !strings.Contains(err.Error(), "--volumes") {
		t.Fatalf("err = %v, want volume-loss gate", err)
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
