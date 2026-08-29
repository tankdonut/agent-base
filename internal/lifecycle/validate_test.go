package lifecycle

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseTagFromDockerfile(t *testing.T) {
	tests := []struct {
		name    string
		docker  string
		want    string
		wantErr string
	}{
		{"plain date tag", "FROM ghcr.io/tankdonut/agent-base:2026.08.28\n", "2026.08.28", ""},
		{"same-day run suffix", "FROM ghcr.io/tankdonut/agent-base:2026.08.24.3\n", "2026.08.24.3", ""},
		{"digest suffix carried", "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc123\n", "2026.08.28@sha256:abc123", ""},
		{"multi-stage alias dropped", "FROM ghcr.io/tankdonut/agent-base:2026.08.28 AS base\n", "2026.08.28", ""},
		{"digest and alias", "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc AS base\n", "2026.08.28@sha256:abc", ""},
		{"indented line", "  FROM ghcr.io/tankdonut/agent-base:2026.08.27\n", "2026.08.27", ""},
		{"no base line", "FROM debian:bookworm\n", "", "no `FROM ghcr.io/tankdonut/agent-base:<tag>` line"},
		{"empty tag", "FROM ghcr.io/tankdonut/agent-base:\n", "", "empty base image tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProject(t, map[string]string{"agent/Dockerfile": tt.docker})
			got, err := BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("tag = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateArgv(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProject(t, map[string]string{
				"agent/Dockerfile":   fixtureDockerfile,
				"agent/spec.json":    tt.spec,
				"agent/.env.example": "#A=1\n",
			})
			r := newFakeRunner("podman")
			if err := Validate(r, "podman", root); err != nil {
				t.Fatal(err)
			}
			if len(r.calls) != 1 {
				t.Fatalf("calls = %v, want exactly one", r.calls)
			}
			want := append([]string{"podman", "run", "--rm", "--env-file", "agent/.env.example"}, tt.wantTail...)
			assertCalls(t, r, [][]string{want})
		})
	}
}

func TestValidateDigestTagCarriedIntoImage(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/Dockerfile":   "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc\n",
		"agent/spec.json":    `{"setup": {"auth_choice": "none"}}`,
		"agent/.env.example": "#A=1\n",
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
}
