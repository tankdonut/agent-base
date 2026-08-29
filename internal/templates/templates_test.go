package templates

import (
	"bytes"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"text/template"
)

// data mirrors the scaffold template surface (scaffold.Config's first
// six fields); templates may reference nothing else.
type data struct {
	ProjectName string
	AgentName   string
	BaseTag     string
	Model       string
	GatewayPort int
	Telegram    bool
}

func sampleData(telegram bool) data {
	return data{
		ProjectName: "my-agent",
		AgentName:   "My Agent",
		BaseTag:     "2026.08.28",
		Model:       "zai/glm-5.2",
		GatewayPort: 18789,
		Telegram:    telegram,
	}
}

// manifest is the exact set of files the tmpl tree must generate.
var manifest = []string{
	".agentctl.yaml",
	".github/workflows/ci.yml",
	".gitignore",
	".markdownlint-cli2.yaml",
	".pre-commit-config.yaml",
	"AGENTS.md",
	"README.md",
	"agent/.env.example",
	"agent/Dockerfile",
	"agent/automations/daily-briefing.md",
	"agent/skills/.gitkeep",
	"agent/spec.json",
	"agent/workspace/AGENTS.md",
	"agent/workspace/MEMORY.md",
	"agent/workspace/SOUL.md",
	"agent/workspace/USER.md",
	"compose.dev.yml",
	"compose.yml",
	"knowledge/content/index.md",
	"make.sh",
	"renovate.json",
}

func mustPaths(t *testing.T) []string {
	t.Helper()
	paths, err := Paths()
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	return paths
}

func TestPathsManifest(t *testing.T) {
	got := mustPaths(t)
	if !reflect.DeepEqual(got, manifest) {
		t.Fatalf("Paths() =\n %v\nwant\n %v", got, manifest)
	}
}

func TestRenderInvariants(t *testing.T) {
	for _, telegram := range []bool{true, false} {
		for _, rel := range mustPaths(t) {
			t.Run(fmt.Sprintf("telegram=%v/%s", telegram, rel), func(t *testing.T) {
				src, err := fs.ReadFile(FS(), "tmpl/"+rel+".tmpl")
				if err != nil {
					t.Fatalf("read template: %v", err)
				}
				tmpl, err := template.New(rel).Parse(string(src))
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				var buf bytes.Buffer
				if err := tmpl.Execute(&buf, sampleData(telegram)); err != nil {
					t.Fatalf("execute: %v", err)
				}
				out := buf.Bytes()
				if strings.Contains(string(out), "{{") {
					t.Errorf("output contains an unrendered template marker:\n%s", out)
				}
				if len(out) > 0 && out[len(out)-1] != '\n' {
					t.Errorf("output does not end with a newline")
				}
			})
		}
	}
}

func TestMode(t *testing.T) {
	tests := []struct {
		path string
		want fs.FileMode
	}{
		{"make.sh", 0o755},
		{"compose.yml", 0o644},
		{"agent/spec.json", 0o644},
		{"agent/.env.example", 0o644},
		{"agent/automations/daily-briefing.md", 0o644},
		{".github/workflows/ci.yml", 0o644},
	}
	for _, tt := range tests {
		if got := Mode(tt.path); got != tt.want {
			t.Errorf("Mode(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
