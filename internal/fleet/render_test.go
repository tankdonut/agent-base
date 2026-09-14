package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sidecarManifest = "agents:\n  grow: {}\n"

const sharedManifest = `plane:
  enabled: true
  name: my-plane
agents:
  grow: {}
`

const overridesManifest = `agents:
  grow:
    overrides:
      volumes:
        - shared-notes:/notes
      ports:
        - 127.0.0.1:9229:9229
      limits:
        cpus: "4.0"
        memory: 6g
        pids: 1024
`

func renderOrDie(t *testing.T, body string, agent string) []byte {
	t.Helper()
	root := writeManifest(t, body)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := RenderAgentCompose(m, agent)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated golden %s", path)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(data); got != want {
		t.Errorf("%s drifted from golden:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestRenderSidecarGolden(t *testing.T) {
	checkGolden(t, "sidecar.yml", string(renderOrDie(t, sidecarManifest, "grow")))
}

func TestRenderSharedGolden(t *testing.T) {
	checkGolden(t, "shared.yml", string(renderOrDie(t, sharedManifest, "grow")))
}

func TestRenderOverridesGolden(t *testing.T) {
	checkGolden(t, "overrides.yml", string(renderOrDie(t, overridesManifest, "grow")))
}

func TestRenderDeterministic(t *testing.T) {
	first := renderOrDie(t, sharedManifest, "grow")
	second := renderOrDie(t, sharedManifest, "grow")
	if string(first) != string(second) {
		t.Error("render is not deterministic across loads")
	}
}

func TestRenderEnvelopeInvariants(t *testing.T) {
	sidecar := string(renderOrDie(t, sidecarManifest, "grow"))
	for _, want := range []string{
		"name: grow\n",
		"127.0.0.1:${AGENT_GATEWAY_PORT:-18789}:18789",
		"agent-data:/home/node/.openclaw",
		"agent-backups:/backups",
		"stop_grace_period: 11m",
		"cap_drop: [ALL]",
		"read_only: true",
	} {
		if !strings.Contains(sidecar, want) {
			t.Errorf("sidecar render missing invariant %q", want)
		}
	}
	shared := string(renderOrDie(t, sharedManifest, "grow"))
	for _, want := range []string{
		"name: my-plane-net",
		"external: true",
		"aliases:",
		"- grow",
	} {
		if !strings.Contains(shared, want) {
			t.Errorf("shared render missing invariant %q", want)
		}
	}
	if strings.Contains(shared, "litellm:") {
		t.Error("shared render must not carry a litellm sidecar")
	}
	if strings.Contains(shared, "model-net") {
		t.Error("shared render must not carry model-net")
	}
}

func TestRenderUnknownAgent(t *testing.T) {
	root := writeManifest(t, sidecarManifest)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RenderAgentCompose(m, "trade")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v, want not-registered", err)
	}
}

func TestRenderAuthoredOptOut(t *testing.T) {
	root := writeManifest(t, "agents:\n  grow:\n    compose_file: compose.custom.yml\n")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RenderAgentCompose(m, "grow")
	if !errors.Is(err, ErrAuthoredCompose) {
		t.Fatalf("err = %v, want ErrAuthoredCompose", err)
	}
}

func TestMaterializeWritesArtifact(t *testing.T) {
	root := writeManifest(t, sharedManifest)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}

	path, err := MaterializeAgentCompose(m, "grow")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(m.Agents["grow"].Dir, RenderedComposeName)
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: grow") {
		t.Errorf("materialized file missing project pin: %q", data)
	}

	stale := []byte("stale content")
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeAgentCompose(m, "grow"); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) == string(stale) {
		t.Error("materialize must regenerate, not keep stale content")
	}
}

func TestOverridesSchemaFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"overrides unknown key", "agents: {grow: {overrides: {turbo: 1}}}\n", "overrides: unknown key"},
		{"limits unknown key", "agents: {grow: {overrides: {limits: {gpu: 1}}}}\n", "limits: unknown key"},
		{"pids must be positive", "agents: {grow: {overrides: {limits: {pids: 0}}}}\n", "positive integer"},
		{"pids must be integer", "agents: {grow: {overrides: {limits: {pids: many}}}}\n", "positive integer"},
		{"ports must be strings", "agents: {grow: {overrides: {ports: [8080]}}}\n", "non-empty string"},
		{"compose_file traversal", "agents: {grow: {compose_file: ../x.yml}}\n", "traverse outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeManifest(t, tt.body)
			_, err := LoadManifest(root)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
