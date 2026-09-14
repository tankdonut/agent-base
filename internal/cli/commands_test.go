package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/tankdonut/agent-base/internal/fleet"
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
	// runOutputs scripts per-argv RunOutput results (joined by spaces);
	// entries here win over runOutputOK.
	runOutputs map[string]string
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
	call := append([]string{name}, args...)
	s.calls = append(s.calls, call)
	if out, ok := s.runOutputs[strings.Join(call, " ")]; ok {
		return []byte(out), nil
	}
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

// writeProject materializes a fixture fleet repo in a temp dir. Files
// are authored root-relative as in the legacy layout; this helper maps
// them into the canonical fleet shape: agent-scoped paths (agent/,
// litellm/, knowledge/, deploy/, compose.dev.yml) land under
// agents/<key>/, compose.yml is dropped (the verb-time render owns it),
// and .agentctl.yaml folds into the synthesized fleet.yaml entry.
func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	// Fixed fixture key: temp-dir basenames are numeric ("001") and an
	// unquoted numeric YAML key changes the decode shape.
	key := "grow"
	prefix := "agents/" + key + "/"
	out := map[string]string{}
	var platform, port, flyApp, flyRegion string
	for rel, content := range files {
		switch {
		case rel == "compose.yml":
			continue
		case rel == ConfigName:
			var cfg map[string]any
			if err := yaml.Unmarshal([]byte(content), &cfg); err == nil {
				if p, ok := cfg["platform"].(string); ok {
					platform = p
				}
				if comp, ok := cfg["compose"].(map[string]any); ok {
					if n, ok := comp["gateway_port"].(int); ok {
						port = strconv.Itoa(n)
					}
				}
				if f, ok := cfg["fly"].(map[string]any); ok {
					flyApp, _ = f["app"].(string)
					flyRegion, _ = f["region"].(string)
				}
			}
			continue
		case rel == "compose.dev.yml" || strings.HasPrefix(rel, "agent/") || strings.HasPrefix(rel, "litellm/") || strings.HasPrefix(rel, "knowledge/") || strings.HasPrefix(rel, "deploy/"):
			// Legacy-form keys: the agent/ wrapper flattens away.
			out[prefix+strings.TrimPrefix(rel, "agent/")] = content
		default:
			out[rel] = content
		}
	}
	manifest := &strings.Builder{}
	manifest.WriteString("agents:\n  " + key + ":\n")
	if platform != "" {
		manifest.WriteString("    platform: " + platform + "\n")
	}
	if port != "" {
		manifest.WriteString("    gateway_port: " + port + "\n")
	}
	if flyApp != "" {
		manifest.WriteString("    fly: {app: " + flyApp)
		if flyRegion != "" {
			manifest.WriteString(", region: " + flyRegion)
		}
		manifest.WriteString("}\n")
	}
	out[fleet.ManifestName] = manifest.String()
	for rel, content := range out {
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

// agentDir returns the fixture's per-agent directory — tests writing
// into a fixture tree address files under it.
func agentDir(root string) string {
	return filepath.Join(root, fleet.AgentsDir, "grow")
}

// fixtureProject materializes a minimal but contract-shaped fleet
// repo: spec with one required env ref, pinned Dockerfile, complete
// .env — the deployment envelope is the verb-time render.
func fixtureProject(t *testing.T) string {
	t.Helper()
	return writeProject(t, map[string]string{
		"agent/spec.json": `{
  "specVersion": 1,
  "setup": {"auth_choice": "zai-coding-global"},
  "model": {"fallback": "{env:FALLBACK_MODEL}"}
}`,
		"agent/Dockerfile":          "FROM ghcr.io/tankdonut/agent-base:2026.09.05\nCOPY agent/spec.json /opt/agent/spec.json\n",
		"agent/.env.example":        "#FALLBACK_MODEL=\n",
		"agent/.env":                "FALLBACK_MODEL=m\nZAI_API_KEY=k\n",
		"agent/automations/jobs.md": "---\nname: probe\ncron: 0 9 * * *\ndeliver: announce\n---\nbody\n",
	})
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
	return execInStdin(t, dir, "", args...)
}

// execInStdin is execIn with scripted stdin (interactive prompts).
func execInStdin(t *testing.T, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	restore := chdir(t, dir)
	defer restore()
	root := NewRootCommand()
	root.SetIn(strings.NewReader(stdin))
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
	if err := os.Remove(filepath.Join(agentDir(root), ".env")); err != nil {
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
	if err := os.WriteFile(filepath.Join(agentDir(root), "Dockerfile"),
		[]byte("FROM ghcr.io/tankdonut/agent-base:2026.09.12\nCOPY agent/spec.json /opt/agent/spec.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(agentDir(root), "spec.json")
	body, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	flipped := strings.Replace(string(body), `"zai-coding-global"`, `"litellm-api-key"`, 1)
	if err := os.WriteFile(spec, []byte(flipped), 0o644); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(agentDir(root), ".env")
	body, err = os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte(string(body)+"LITELLM_API_KEY=sk-doctor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir(root), "litellm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir(root), "litellm", ".env.example"), []byte("#LITELLM_MASTER_KEY=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir(root), "litellm", ".env"), []byte("LITELLM_MASTER_KEY=sk-doctor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// sharedLiteLLMFixture flips the manifest to shared plane placement —
// the fleet-world shape gap doctor must FAIL on (the plane compose
// render ships with P2).
func sharedLiteLLMFixture(t *testing.T) string {
	t.Helper()
	root := litellmFixture(t)
	body := "plane:\n  enabled: true\n  name: p-plane\n  litellm: shared\nagents:\n  grow:\n    litellm: shared\n"
	if err := os.WriteFile(filepath.Join(root, fleet.ManifestName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
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

func TestDoctorLitellmShapeFailsOnSharedPlane(t *testing.T) {
	root := sharedLiteLLMFixture(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor")
	if err == nil {
		t.Fatal("doctor must fail when a litellm spec rides the not-yet-rendered shared plane")
	}
	if !strings.Contains(out, "agents.grow.litellm: shared but the plane compose render ships with P2") {
		t.Errorf("output lacks the shape FAIL line:\n%s", out)
	}
}

func TestDoctorLitellmShapePasses(t *testing.T) {
	root := litellmFixture(t)
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
	if err := os.WriteFile(filepath.Join(agentDir(root), "Dockerfile"),
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

func TestDoctorIgnoresHarnessTagEra(t *testing.T) {
	root := litellmFixture(t)
	if err := os.WriteFile(filepath.Join(agentDir(root), "Dockerfile"),
		[]byte("FROM ghcr.io/tankdonut/agent-base:2000.01.01\nCOPY agent/spec.json /opt/agent/spec.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := stubbedRunner(t, "podman")
	r.runOutputOK = true
	out, err := execIn(t, root, "doctor")
	if err != nil {
		t.Fatalf("harness tags are local-build overrides, not eras: %v\n%s", err, out)
	}
	if strings.Contains(out, "predates the litellm seed") {
		t.Errorf("year-2000 tag must not trip the era check:\n%s", out)
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
	if err := os.Remove(filepath.Join(agentDir(root), ".env")); err != nil {
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

func TestBackupDrivesInInstancePrimitive(t *testing.T) {
	root := fixtureProject(t)
	r := stubbedRunner(t, "podman")
	out, err := execIn(t, root, "backup")
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	want := []string{"podman", "compose", "-f", "compose.yml", "exec", "agent",
		"openclaw", "backup", "create", "--verify", "--output", "/backups"}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
	for i, arg := range want {
		if r.calls[0][i] != arg {
			t.Fatalf("call = %v, want %v", r.calls[0], want)
		}
	}
	if !strings.Contains(out, "agent-backups") {
		t.Errorf("output lacks the archive location: %q", out)
	}
}

func TestBackupFlyUsesSSHConsole(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json":    `{"specVersion": 1, "setup": {"auth_choice": "zai-coding-global"}}`,
		"agent/Dockerfile":   "FROM ghcr.io/tankdonut/agent-base:2026.09.05\nCOPY agent/spec.json /opt/agent/spec.json\n",
		"agent/.env.example": "#FALLBACK_MODEL=\n",
		"agent/.env":         "FALLBACK_MODEL=m\nZAI_API_KEY=k\n",
		".agentctl.yaml":     "platform: fly\n",
	})
	if err := fly.ScaffoldConfig(agentDir(root), "my-agent", "sjc"); err != nil {
		t.Fatal(err)
	}
	r := stubbedRunner(t, "podman", "fly")
	out, err := execIn(t, root, "backup")
	if err != nil {
		t.Fatalf("backup (fly): %v\n%s", err, out)
	}
	want := [][]string{
		{"fly", "ssh", "console", "-a", "my-agent", "-C", "openclaw backup create --verify --output /backups"},
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
	manifest, err := os.ReadFile(filepath.Join(agentDir(root), "deploy", "fly.toml"))
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
	fleetData, err := os.ReadFile(filepath.Join(root, fleet.ManifestName))
	if err != nil || !strings.Contains(string(fleetData), "platform: fly") {
		t.Errorf("platform not pinned in fleet.yaml: %v %s", err, fleetData)
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
	root := writeProject(t, map[string]string{
		"agent/spec.json":    `{"specVersion": 1, "setup": {"auth_choice": "zai-coding-global"}}`,
		"agent/Dockerfile":   "FROM ghcr.io/tankdonut/agent-base:2026.09.05\nCOPY agent/spec.json /opt/agent/spec.json\n",
		"agent/.env.example": "#FALLBACK_MODEL=\n",
		"agent/.env":         "FALLBACK_MODEL=m\nZAI_API_KEY=k\n",
		".agentctl.yaml":     "platform: fly\n",
	})
	stubbedRunner(t, "podman", "fly")
	if err := fly.ScaffoldConfig(agentDir(root), "my-agent", "sjc"); err != nil {
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
	data, err := os.ReadFile(filepath.Join(root, fleet.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "platform: compose") {
		t.Errorf("fleet.yaml = %q", data)
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

// The contractless-manifest negative case is structurally unreachable
// in fleet repos: every verb re-renders the envelope from fleet.yaml,
// so a hand-planted compose.yml cannot drift into a contractless
// shape. Volume presence is pinned by the renderer goldens.

func TestReleaseVerbsNeedProject(t *testing.T) {
	stubbedRunner(t, "podman")
	if _, err := execIn(t, t.TempDir(), "status"); err == nil {
		t.Fatal("status outside a project must fail")
	}
}
