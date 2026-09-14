package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/fleet"
)

// legacyRepo builds a realistic pre-migration single-agent repo. The
// root is a named subdir (basename "grow") so the migrated agent key is
// deterministic — temp-dir basenames are numeric.
func legacyRepo(t *testing.T, extra func(root string)) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "grow")
	files := map[string]string{
		"agent/spec.json":            `{"specVersion": 1, "agent": {"name": "Grow"}, "setup": {"auth_choice": "zai-coding-global"}}`,
		"agent/Dockerfile":           "FROM ghcr.io/tankdonut/agent-base:2026.09.05\n",
		"agent/.env.example":         "#ZAI_API_KEY=\n",
		"agent/.env":                 "ZAI_API_KEY=k\n",
		"agent/workspace/SOUL.md":    "persona\n",
		"agent/automations/jobs.md":  "---\nname: probe\ncron: 0 9 * * *\ndeliver: announce\n---\nbody\n",
		"knowledge/content/index.md": "docs\n",
		"litellm/config.yaml":        "model_list: []\n",
		"compose.yml": `name: grow
services:
  agent:
    build: {context: ., dockerfile: agent/Dockerfile}
    ports:
      - "127.0.0.1:${AGENT_GATEWAY_PORT:-18795}:18789"
`,
		"compose.dev.yml": "services:\n  agent:\n    environment:\n      - AGENT_SKIP_SEED=1\n",
		"README.md":       "# grow\n",
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
	if extra != nil {
		extra(root)
	}
	return root
}

func runMigrateCmd(t *testing.T, args ...string) (int, string) {
	t.Helper()
	root := NewRootCommand()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	root.SilenceErrors = true
	root.SetArgs(append([]string{"migrate"}, args...))
	if err := root.Execute(); err != nil {
		return 1, out.String() + err.Error() + "\n"
	}
	return 0, out.String()
}

func TestMigrateRestructuresAndSynthesizes(t *testing.T) {
	root := legacyRepo(t, func(root string) {
		if err := os.WriteFile(filepath.Join(root, ConfigName), []byte("platform: compose\ncompose:\n  gateway_port: 18795\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	restore := chdir(t, root)
	defer restore()

	code, out := runMigrateCmd(t)
	if code != 0 {
		t.Fatalf("migrate exit = %d:\n%s", code, out)
	}

	key := "grow"
	for _, rel := range []string{
		filepath.Join(fleet.AgentsDir, key, "spec.json"),
		filepath.Join(fleet.AgentsDir, key, "workspace", "SOUL.md"),
		filepath.Join(fleet.AgentsDir, key, "automations", "jobs.md"),
		filepath.Join(fleet.AgentsDir, key, "knowledge", "content", "index.md"),
		filepath.Join(fleet.AgentsDir, key, "litellm", "config.yaml"),
		filepath.Join(fleet.AgentsDir, key, "compose.dev.yml"),
		"README.md",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s missing after migrate: %v", rel, err)
		}
	}
	for _, gone := range []string{"agent", "litellm", "knowledge", "compose.yml", ConfigName} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s must not remain at the repo root after migrate", gone)
		}
	}

	manifest, err := os.ReadFile(filepath.Join(root, fleet.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	got := string(manifest)
	for _, want := range []string{"agents:", `"grow":`, "    gateway_port: 18795"} {
		if !strings.Contains(got, want) {
			t.Errorf("fleet.yaml missing %q:\n%s", want, got)
		}
	}

	ignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil || !strings.Contains(string(ignore), "agents/*/compose.yml") {
		t.Errorf(".gitignore missing the rendered-envelope pattern: %v\n%s", err, ignore)
	}

	// The migrated repo must be immediately operable: fleet check loads
	// it, and the verb path resolves the fleet-of-one.
	code, out = runFleet(t, "fleet", "check")
	if code != 0 {
		t.Fatalf("fleet check on the migrated repo failed:\n%s", out)
	}
	if !strings.Contains(out, "1 agent(s), 0 service(s), plane off — 0 error(s)") {
		t.Errorf("fleet check summary wrong:\n%s", out)
	}
}

func TestMigrateRecoversPortFromComposeWhenConfigAbsent(t *testing.T) {
	root := legacyRepo(t, nil)
	restore := chdir(t, root)
	defer restore()

	if code, out := runMigrateCmd(t); code != 0 {
		t.Fatalf("migrate exit = %d:\n%s", code, out)
	}
	manifest, err := os.ReadFile(filepath.Join(root, fleet.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "gateway_port: 18795") {
		t.Errorf("port not recovered from the compose default:\n%s", manifest)
	}
	// No .agentctl.yaml to fold: the retired file must not reappear.
	if _, err := os.Stat(filepath.Join(root, ConfigName)); !os.IsNotExist(err) {
		t.Errorf("%s recreated despite no legacy config", ConfigName)
	}
}

func TestMigrateRefusesFleetRepoAndRepeats(t *testing.T) {
	t.Run("already a fleet repo", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, fleet.ManifestName), []byte("agents: {grow: {}}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "agents", "grow", "agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agents", "grow", "spec.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		restore := chdir(t, dir)
		defer restore()

		code, out := runMigrateCmd(t)
		if code != 1 || !strings.Contains(out, "nothing to migrate") {
			t.Fatalf("code = %d out = %q, want already-fleet error", code, out)
		}
	})

	t.Run("second run on a migrated repo refuses", func(t *testing.T) {
		root := legacyRepo(t, nil)
		restore := chdir(t, root)
		defer restore()
		if code, out := runMigrateCmd(t); code != 0 {
			t.Fatalf("first migrate failed:\n%s", out)
		}
		code, out := runMigrateCmd(t)
		if code != 1 || !strings.Contains(out, "nothing to migrate") {
			t.Fatalf("code = %d out = %q, want already-fleet error", code, out)
		}
	})
}

func TestMigrateFromSubdirectoryFindsLegacyRoot(t *testing.T) {
	root := legacyRepo(t, nil)
	restore := chdir(t, filepath.Join(root, "agent"))
	defer restore()

	if code, out := runMigrateCmd(t); code != 0 {
		t.Fatalf("migrate from subdir exit = %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, fleet.ManifestName)); err != nil {
		t.Errorf("manifest not written at the legacy root: %v", err)
	}
}
