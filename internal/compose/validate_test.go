package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateArgv(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		scripts  bool
		wantTail []string // the -e block, image, and --validate-spec
	}{
		{
			name: "zai auth adds gated key after sorted refs",
			spec: fixtureSpec,
			wantTail: []string{
				"-e", "FALLBACK_MODEL=dummy",
				"-e", "PROVIDER_KEY=dummy",
				"-e", "TELEGRAM_ALLOWED_USERS=dummy",
				"-e", "ZAI_API_KEY=dummy",
				"ghcr.io/tankdonut/agent-base:2026.08.28", "--validate-spec",
			},
		},
		{
			name: "non-zai auth adds no ZAI key",
			spec: `{"setup": {"auth_choice": "anthropic"}, "config": [{"path": "x", "value": "{env:FOO}"}]}`,
			wantTail: []string{
				"-e", "FOO=dummy",
				"ghcr.io/tankdonut/agent-base:2026.08.28", "--validate-spec",
			},
		},
		{
			name: "explicit ZAI ref is not duplicated",
			spec: `{"setup": {"auth_choice": "zai-coding-global"}, "config": [{"path": "x", "value": "{env:ZAI_API_KEY}"}]}`,
			wantTail: []string{
				"-e", "ZAI_API_KEY=dummy",
				"ghcr.io/tankdonut/agent-base:2026.08.28", "--validate-spec",
			},
		},
		{
			name: "litellm auth adds gated key after sorted refs",
			spec: `{"setup": {"auth_choice": "litellm-api-key"}, "config": [{"path": "x", "value": "{env:FOO}"}]}`,
			wantTail: []string{
				"-e", "FOO=dummy",
				"-e", "LITELLM_API_KEY=dummy",
				"ghcr.io/tankdonut/agent-base:2026.08.28", "--validate-spec",
			},
		},
		{
			name: "litellm near-miss auth adds no key",
			spec: `{"setup": {"auth_choice": "litellm"}, "config": [{"path": "x", "value": "{env:FOO}"}]}`,
			wantTail: []string{
				"-e", "FOO=dummy",
				"ghcr.io/tankdonut/agent-base:2026.08.28", "--validate-spec",
			},
		},
		{
			name: "shipped scripts sibling is mounted read-only",
			spec: fixtureSpec,
			wantTail: []string{
				"-e", "FALLBACK_MODEL=dummy",
				"-e", "PROVIDER_KEY=dummy",
				"-e", "TELEGRAM_ALLOWED_USERS=dummy",
				"-e", "ZAI_API_KEY=dummy",
				"ghcr.io/tankdonut/agent-base:2026.08.28", "--validate-spec",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{
				"agent/Dockerfile":          fixtureDockerfile,
				"agent/spec.json":           tt.spec,
				"agent/.env.example":        "#A=1\n",
				"agent/automations/jobs.md": "---\nname: probe\n---\nbody\n",
			}
			if tt.name == "shipped scripts sibling is mounted read-only" {
				files["agent/scripts/probe.py"] = "pass\n"
			}
			root := writeProject(t, files)
			r := newFakeRunner("podman")
			if err := Validate(r, "podman", root); err != nil {
				t.Fatal(err)
			}
			if len(r.calls) != 1 {
				t.Fatalf("calls = %v, want exactly one", r.calls)
			}
			want := []string{
				"podman", "run", "--rm",
				"--security-opt", "label=disable",
				"-v", filepath.Join(root, "agent", "spec.json") + ":/opt/agent/spec.json:ro",
				"-v", filepath.Join(root, "agent", "automations") + ":/opt/agent/automations:ro",
			}
			if tt.name == "shipped scripts sibling is mounted read-only" {
				want = append(want, "-v", filepath.Join(root, "agent", "scripts")+":/opt/agent/scripts:ro")
			}
			want = append(want, "--env-file", "agent/.env.example")
			want = append(want, tt.wantTail...)
			assertCalls(t, r.calls, [][]string{want})
		})
	}
}

func TestValidateDigestTagCarriedIntoImage(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/Dockerfile":          "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc\n",
		"agent/spec.json":           `{"setup": {"auth_choice": "none"}}`,
		"agent/.env.example":        "#A=1\n",
		"agent/automations/jobs.md": "---\nname: probe\n---\nbody\n",
	})
	r := newFakeRunner("docker")
	if err := Validate(r, "docker", root); err != nil {
		t.Fatal(err)
	}
	image := r.calls[0][len(r.calls[0])-2]
	if image != "ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc" {
		t.Errorf("image = %q, want digest-suffixed reference", image)
	}
}

func TestValidateMissingInputs(t *testing.T) {
	specOnly := writeProject(t, map[string]string{"agent/spec.json": "{}", "agent/.env.example": "#A=1\n"})
	if err := Validate(newFakeRunner("podman"), "podman", specOnly); err == nil || !strings.Contains(err.Error(), "Dockerfile") {
		t.Errorf("missing Dockerfile: err = %v, want Dockerfile error", err)
	}
	noExample := writeProject(t, map[string]string{"agent/spec.json": "{}", "agent/Dockerfile": fixtureDockerfile})
	if err := Validate(newFakeRunner("podman"), "podman", noExample); err == nil || !strings.Contains(err.Error(), ".env.example") {
		t.Errorf("missing .env.example: err = %v, want .env.example error", err)
	}
	noAutomations := writeProject(t, map[string]string{
		"agent/Dockerfile":   fixtureDockerfile,
		"agent/spec.json":    "{}",
		"agent/.env.example": "#A=1\n",
	})
	if err := Validate(newFakeRunner("podman"), "podman", noAutomations); err == nil || !strings.Contains(err.Error(), "agent/automations") {
		t.Errorf("missing automations: err = %v, want agent/automations error", err)
	}
	// A file at the automations path is not a directory: fail the same way.
	notDir := writeProject(t, map[string]string{
		"agent/Dockerfile":          fixtureDockerfile,
		"agent/spec.json":           "{}",
		"agent/.env.example":        "#A=1\n",
		"agent/automations/jobs.md": "---\nname: probe\n---\nbody\n",
	})
	// Remove the directory but keep a file in its place.
	if err := os.RemoveAll(filepath.Join(notDir, "agent", "automations")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notDir, "agent", "automations"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(newFakeRunner("podman"), "podman", notDir); err == nil || !strings.Contains(err.Error(), "agent/automations") {
		t.Errorf("file at automations path: err = %v, want agent/automations error", err)
	}
}
