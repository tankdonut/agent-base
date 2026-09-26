package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ManifestName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadManifestFleetOfOneDefaults(t *testing.T) {
	root := writeManifest(t, "agents:\n  grow: {}\n")

	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Plane.Enabled {
		t.Error("plane enabled by default; want disabled for a fresh fleet-of-one")
	}
	if got := m.Plane.GatewayBasePort; got != DefaultGatewayBasePort {
		t.Errorf("base port = %d, want %d", got, DefaultGatewayBasePort)
	}
	grow := m.Agents["grow"]
	if grow.GatewayPort != DefaultGatewayBasePort {
		t.Errorf("grow port = %d, want allocated base %d", grow.GatewayPort, DefaultGatewayBasePort)
	}
	if grow.ExplicitPort {
		t.Error("grow port marked explicit")
	}
	if want := filepath.Join(root, AgentsDir, "grow"); grow.Dir != want {
		t.Errorf("grow dir = %q, want %q", grow.Dir, want)
	}
	if grow.LiteLLMUsed != LiteLLMSidecar {
		t.Errorf("grow litellm = %q, want sidecar (plane disabled)", grow.LiteLLMUsed)
	}
	if got := m.AgentNames(); len(got) != 1 || got[0] != "grow" {
		t.Errorf("agent names = %v, want [grow]", got)
	}
}

func TestLoadManifestFull(t *testing.T) {
	root := writeManifest(t, `plane:
  enabled: true
  name: my-plane
  gateway_base_port: 20000
  litellm: shared
defaults:
  compose:
    engine: podman
  plugins:
    - name: diagnostics-prometheus
  mcp_servers:
    - name: memory
      url: http://memory-mcp:3000
      transport: streamable-http
services:
  browser-mcp:
    networks: [fleet-net]
agents:
  grow:
    gateway_port: 20005
  trade:
    platform: fly
    fly: {app: trade-agent, region: iad}
`)

	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Plane.Enabled || m.Plane.Name != "my-plane" || m.Plane.GatewayBasePort != 20000 {
		t.Errorf("plane = %+v", m.Plane)
	}
	if m.Defaults.ComposeEngine != "podman" {
		t.Errorf("engine = %q, want podman", m.Defaults.ComposeEngine)
	}
	if len(m.Defaults.Plugins) != 1 || m.Defaults.Plugins[0].Name != "diagnostics-prometheus" {
		t.Errorf("plugins = %+v", m.Defaults.Plugins)
	}
	if len(m.Defaults.McpServers) != 1 || m.Defaults.McpServers[0].Name != "memory" {
		t.Errorf("mcp = %+v", m.Defaults.McpServers)
	}
	svc := m.Services["browser-mcp"]
	if want := filepath.Join(root, ServicesDir, "browser-mcp"); svc.Dir != want {
		t.Errorf("service dir = %q, want %q", svc.Dir, want)
	}
	if len(svc.Networks) != 1 || svc.Networks[0] != "fleet-net" {
		t.Errorf("service networks = %v", svc.Networks)
	}
	grow := m.Agents["grow"]
	if grow.GatewayPort != 20005 || !grow.ExplicitPort {
		t.Errorf("grow = %+v", grow)
	}
	if grow.LiteLLMUsed != LiteLLMShared {
		t.Errorf("grow litellm = %q, want shared (plane provides it)", grow.LiteLLMUsed)
	}
	trade := m.Agents["trade"]
	if trade.FlyApp != "trade-agent" || trade.FlyRegion != "iad" {
		t.Errorf("trade fly = %+v", trade)
	}
	// Allocation order is sorted names, so trade (no explicit port)
	// gets the base — not its later position in the document.
	if trade.GatewayPort != 20000 {
		t.Errorf("trade port = %d, want base 20000 (sorted-name allocation)", trade.GatewayPort)
	}
}

func TestLoadManifestOverridesImage(t *testing.T) {
	root := writeManifest(t, `agents:
  grow:
    overrides:
      image: ghcr.io/tankdonut/agent-base-staging@sha256:abc123
`)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Agents["grow"].Overrides
	if got == nil || got.Image != "ghcr.io/tankdonut/agent-base-staging@sha256:abc123" {
		t.Fatalf("overrides.image = %+v, want the pinned ref", got)
	}

	for name, body := range map[string]string{
		"empty":  `agents: {grow: {overrides: {image: ""}}}`,
		"spaced": "agents: {grow: {overrides: {image: \"repo/app --platform linux/amd64\"}}}",
	} {
		root := writeManifest(t, body)
		if _, err := LoadManifest(root); err == nil {
			t.Fatalf("%s: overrides.image accepted a invalid value", name)
		}
	}
}

func TestLoadManifestSidecarOptOutWithPlane(t *testing.T) {
	root := writeManifest(t, `plane:
  enabled: true
  name: my-plane
agents:
  grow: {}
  isolated:
    litellm: sidecar
`)

	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Agents["grow"].LiteLLMUsed; got != LiteLLMShared {
		t.Errorf("grow litellm = %q, want inherited shared", got)
	}
	if got := m.Agents["isolated"].LiteLLMUsed; got != LiteLLMSidecar {
		t.Errorf("isolated litellm = %q, want explicit sidecar opt-out honored", got)
	}
}

func TestLoadManifestAllocationDeterministicAndSkipsTaken(t *testing.T) {
	// Document order deliberately differs from sorted order; explicit
	// port sits exactly on the base to prove allocation steps past it.
	root := writeManifest(t, `agents:
  zulu: {}
  alpha:
    gateway_port: 18789
  mike: {}
`)

	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.AgentNames(); strings.Join(got, ",") != "alpha,mike,zulu" {
		t.Errorf("names = %v, want sorted", got)
	}
	if m.Agents["alpha"].GatewayPort != 18789 {
		t.Errorf("alpha explicit port lost")
	}
	if m.Agents["mike"].GatewayPort != 18790 {
		t.Errorf("mike port = %d, want 18790 (skips the taken base)", m.Agents["mike"].GatewayPort)
	}
	if m.Agents["zulu"].GatewayPort != 18791 {
		t.Errorf("zulu port = %d, want 18791", m.Agents["zulu"].GatewayPort)
	}
}

func TestLoadManifestUnknownKeysFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"top-level unknown", "agents: {grow: {}}\nturbo: true\n", "unknown key"},
		{"legacy platform key names its rename", "platform: compose\n", "agents.<name>.platform"},
		{"legacy compose key names its rename", "compose: {engine: podman}\n", "defaults.compose.engine"},
		{"legacy fly key names its rename", "fly: {app: x}\n", "agents.<name>.fly"},
		{"plane unknown key", "plane: {turbo: true}\n", "plane: unknown key"},
		{"agent unknown key", "agents: {grow: {turbo: true}}\n", "agents.grow: unknown key"},
		{"defaults unknown key", "defaults: {turbo: true}\nagents: {grow: {}}\n", "defaults: unknown key"},
		{"fly unknown key", "agents: {grow: {fly: {app: a, turbo: 1}}}\n", "fly: unknown key"},
		{"service unknown key", "services: {db: {turbo: 1}}\nagents: {grow: {}}\n", "services.db: unknown key"},
		{"mcp unknown key", "defaults: {mcp_servers: [{name: m, url: http://x, turbo: 1}]}\nagents: {grow: {}}\n", "unknown key"},
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

func TestLoadManifestFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"no agents at all", "plane: {enabled: false}\n", "no agents registered"},
		{"empty agents mapping", "agents: {}\n", "no agents registered"},
		{"agent name must be lowercase-safe", "agents: {Grow: {}}\n", "invalid name"},
		{"agent name rejects traversal", "agents: {\"../evil\": {}}\n", "invalid name"},
		{"duplicate explicit ports", "agents: {a: {gateway_port: 19000}, b: {gateway_port: 19000}}\n", "already assigned to agent"},
		{"port out of range", "agents: {a: {gateway_port: 70000}}\n", "out of range"},
		{"port zero", "agents: {a: {gateway_port: 0}}\n", "out of range"},
		{"shared litellm without plane", "agents: {a: {litellm: shared}}\n", "requires an enabled plane"},
		{"shared litellm with observability-only plane", "plane: {enabled: true, name: p, litellm: none}\nagents: {a: {litellm: shared}}\n", "requires an enabled plane"},
		{"plane enabled without name", "plane: {enabled: true}\nagents: {a: {}}\n", "plane.name is required"},
		{"plane bad litellm value", "plane: {enabled: true, name: p, litellm: sidecar}\nagents: {a: {}}\n", `want "shared" or "none"`},
		{"agent bad litellm value", "agents: {a: {litellm: turbo}}\n", `want "shared" or "sidecar"`},
		{"fly block without app", "agents: {a: {fly: {region: iad}}}\n", "fly.app is required"},
		{"service dir absolute", "services: {db: {dir: /etc}}\nagents: {a: {}}\n", "relative to the fleet root"},
		{"service dir traversal", "services: {db: {dir: ../db}}\nagents: {a: {}}\n", "traverse outside"},
		{"mcp both command and url", "defaults: {mcp_servers: [{name: m, command: x, url: http://y}]}\nagents: {a: {}}\n", "mutually exclusive"},
		{"mcp neither command nor url", "defaults: {mcp_servers: [{name: m}]}\nagents: {a: {}}\n", "exactly one of command"},
		{"mcp missing name", "defaults: {mcp_servers: [{url: http://y}]}\nagents: {a: {}}\n", "name is required"},
		{"plugin missing name", "defaults: {plugins: [{source: /tmp/p}]}\nagents: {a: {}}\n", "name is required"},
		{"engine empty string", "defaults: {compose: {engine: \"\"}}\nagents: {a: {}}\n", "non-empty string"},
		{"plane not a mapping", "plane: hi\nagents: {a: {}}\n", "want a mapping"},
		{"agents not a mapping", "agents: [a]\n", "want a mapping"},
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

func TestLoadManifestBareAgentEntry(t *testing.T) {
	root := writeManifest(t, "agents:\n  grow:\n")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.AgentNames(); len(got) != 1 || got[0] != "grow" {
		t.Errorf("names = %v", got)
	}
	if m.Agents["grow"].GatewayPort != DefaultGatewayBasePort {
		t.Errorf("port = %d", m.Agents["grow"].GatewayPort)
	}
}

func TestLoadManifestNumericAgentNameQuoted(t *testing.T) {
	// An unquoted numeric key changes yaml's decode shape to
	// map[any]any; the loader normalizes instead of failing with a
	// confusing type error. A quoted "001" is a legal agent name.
	root := writeManifest(t, "agents:\n  \"001\": {}\n")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Agents["001"]; !ok {
		t.Fatalf("agent 001 missing: %+v", m.Agents)
	}
	if m.Agents["001"].GatewayPort != DefaultGatewayBasePort {
		t.Errorf("port = %d", m.Agents["001"].GatewayPort)
	}
}

func TestLoadManifestMissingFile(t *testing.T) {
	_, err := LoadManifest(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("err = %v, want read error", err)
	}
}

func TestFindRootWalksUp(t *testing.T) {
	root := writeManifest(t, "agents: {grow: {}}\n")
	nested := filepath.Join(root, AgentsDir, "grow", "agent")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Errorf("FindRoot = %q, want %q", got, root)
	}
}

func TestFindRootAbsent(t *testing.T) {
	_, err := FindRoot(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), ManifestName) {
		t.Fatalf("err = %v, want missing-marker error", err)
	}
}

func TestMcpEntryRawCarriesVerbatim(t *testing.T) {
	root := writeManifest(t, `defaults:
  mcp_servers:
    - name: fetch
      command: uvx
      args: [mcp-server-fetch]
      env: {API_KEY: "{env:MCP_FETCH_KEY}"}
agents:
  a: {}
`)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	raw := m.Defaults.McpServers[0].Raw
	if raw["command"] != "uvx" {
		t.Errorf("raw command = %v", raw["command"])
	}
	env := raw["env"].(map[string]any)
	if env["API_KEY"] != "{env:MCP_FETCH_KEY}" {
		t.Errorf("raw env = %v, want the unresolved {env:VAR} token", env)
	}
}
