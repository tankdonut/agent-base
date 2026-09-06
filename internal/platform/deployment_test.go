package platform

import (
	"os"
	"path/filepath"
	"testing"
)

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

const fixtureDockerfile = "FROM ghcr.io/tankdonut/agent-base:2026.08.28\nCOPY agent/spec.json /opt/agent/spec.json\n"

const fixtureSpec = `{
  "specVersion": 1,
  "model": {"fallback": "{env:FALLBACK_MODEL}"},
  "mcp_servers": [{"url": "https://example.test/mcp?key={env:PROVIDER_KEY}"}]
}`

func TestDerive(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json":  fixtureSpec,
		"agent/Dockerfile": fixtureDockerfile,
		"agent/.env":       "FALLBACK_MODEL=m\nPROVIDER_KEY=k\n",
		"compose.yml":      "name: pinned-name\nservices:\n  agent: {}\n",
	})
	d, err := Derive(root)
	if err != nil {
		t.Fatal(err)
	}
	if d.BaseTag != "2026.08.28" {
		t.Errorf("BaseTag = %q", d.BaseTag)
	}
	if d.Project != "pinned-name" {
		t.Errorf("Project = %q, want the compose.yml pin (not the basename)", d.Project)
	}
	if len(d.EnvKeys) != 2 || d.EnvKeys[0] != "FALLBACK_MODEL" || d.EnvKeys[1] != "PROVIDER_KEY" {
		t.Errorf("EnvKeys = %v", d.EnvKeys)
	}
	if len(d.SpecRefs) != 2 {
		t.Errorf("SpecRefs = %v", d.SpecRefs)
	}
	if !filepath.IsAbs(d.Root) {
		t.Errorf("Root = %q, want absolute", d.Root)
	}
}

func TestDeriveProjectNameFallsBackToBasename(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json":  fixtureSpec,
		"agent/Dockerfile": fixtureDockerfile,
	})
	d, err := Derive(root)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(root)
	want := sanitizeName(base)
	if d.Project != want {
		t.Errorf("Project = %q, want sanitized basename %q", d.Project, want)
	}
}

func TestDeriveWithoutEnvFile(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json":  fixtureSpec,
		"agent/Dockerfile": fixtureDockerfile,
	})
	d, err := Derive(root)
	if err != nil {
		t.Fatal(err)
	}
	if d.EnvKeys != nil {
		t.Errorf("EnvKeys = %v, want nil", d.EnvKeys)
	}
}

func TestDeriveFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		missing string
	}{
		{"no dockerfile pin", "agent/Dockerfile"},
		{"no spec", "agent/spec.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProject(t, map[string]string{
				"agent/spec.json":  fixtureSpec,
				"agent/Dockerfile": fixtureDockerfile,
			})
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(tt.missing))); err != nil {
				t.Fatal(err)
			}
			if _, err := Derive(root); err == nil {
				t.Fatal("Derive must fail closed on an incomplete project")
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("My.Agent"); got != "my-agent" {
		t.Errorf("sanitizeName = %q, want my-agent", got)
	}
}
