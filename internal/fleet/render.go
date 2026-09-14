package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// ErrAuthoredCompose reports an agent whose compose_file opt-out means
// agentctl must not render or overwrite its deployment envelope.
var ErrAuthoredCompose = errors.New("agent uses an authored compose_file — rendering skipped")

// RenderedDir is the per-agent agentctl-owned namespace for host-side
// state agentctl generates besides the envelope (drift markers,
// migrate/check records): agents/<name>/.agentctl/ — one gitignore
// pattern covers everything in it.
const RenderedDir = ".agentctl"

// RenderedComposeName is the materialized envelope filename at the
// AGENT DIR ROOT. The envelope deliberately does not live inside
// .agentctl/: docker compose and podman-compose resolve context,
// dockerfile, env_file, and bind-mount paths against different bases
// (project dir vs compose-file dir), and those bases agree only when
// the file sits at the build-context root.
const RenderedComposeName = "compose.yml"

// litellmSidecarImage mirrors the scaffold's committed pin; fleet
// releases bump it exactly like the template used to.
const litellmSidecarImage = "ghcr.io/berriai/litellm:v1.100.0@sha256:c8756e7b9a61fe45df2ccb5b781d388c3b2f3a21ef9e4956630caef20f9f03aa"

// PlaneNetworkName is the external network the plane owns and
// plane-attached agents join with a per-agent alias.
func PlaneNetworkName(planeName string) string {
	return planeName + "-net"
}

// renderModel carries everything the envelope templates interpolate.
type renderModel struct {
	Project      string
	GatewayPort  int
	PlaneNetwork string
	Shared       bool
	Volumes      []string
	Ports        []string
	Cpus         string
	Memory       string
	Pids         string
	LitellmImage string
}

var sidecarTemplate = template.Must(template.New("sidecar").Parse(`# RENDERED by agentctl from fleet.yaml — DO NOT EDIT.
# The envelope (pinned project name, volumes, networks, hardening) is
# owned by the fleet renderer; changes belong in fleet.yaml or the
# renderer, not here. Regenerated on every fleet verb.

# Pinned compose project name: every generated resource (containers,
# volumes, the agent-net network) is prefixed with it, so the stack is
# immune to directory renames and cannot collide with another agent on
# the same host. Real volume names are <project>_agent-data/-backups.
name: {{.Project}}

services:
  agent:
    build:
      context: .
      dockerfile: Dockerfile
    restart: unless-stopped
    # Keep the engine stop timeout above AGENT_SHUTDOWN_GRACE (600s
    # default) or the engine SIGKILLs the gateway process group
    # mid-drain.
    stop_grace_period: 11m
    # Hardening baseline: no privilege escalation, no capabilities,
    # read-only root filesystem (writable paths are the named volumes
    # plus tmpfs /tmp). Label confinement is disabled deliberately:
    # rootless podman assigns each up a fresh MCS category pair, so
    # warm-volume files from a previous generation would hit
    # PermissionError under label enforcement.
    security_opt:
      - no-new-privileges:true
      - label=disable
    cap_drop: [ALL]
    read_only: true
    tmpfs:
      - /tmp
    env_file:
      - .env
    networks:
      - agent-net
      - model-net
    depends_on:
      - litellm
    volumes:
      - agent-data:/home/node/.openclaw
      # Verified upgrade backups land here before any image-version
      # delta mutates a warm volume.
      - agent-backups:/backups
{{- range .Volumes }}
      - {{ . }}
{{- end }}
    # Loopback-only bind; remote access goes through a TLS-terminating
    # reverse proxy. AGENT_GATEWAY_PORT is host-side compose
    # interpolation and never enters the container; the fleet manifest
    # owns it (an AGENT_GATEWAY_PORT line in .env is an error).
    ports:
      - "127.0.0.1:${AGENT_GATEWAY_PORT:-{{.GatewayPort}}}:18789"
{{- range .Ports }}
      - "{{ . }}"
{{- end }}
    healthcheck:
      test: ["CMD", "node", "-e", "fetch('http://localhost:18789/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"]
      interval: 30s
      timeout: 10s
      retries: 3
      # First boot runs setup + reconcile + seed before the gateway
      # starts; upgrade boots add a verified backup.
      start_period: 300s
    deploy:
      resources:
        limits:
          cpus: "{{.Cpus}}"
          memory: {{.Memory}}
          pids: {{.Pids}}
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "5"

  # Model/provider home: EVERY provider API key lives only in
  # litellm/.env — the agent container never sees them and holds only
  # the proxy key. DB-less mode: the master key is the whole auth
  # surface (no virtual keys/budgets — those need the plane's shared
  # proxy). Version tag is pinned by the fleet renderer.
  litellm:
    image: {{.LitellmImage}}
    command: ["--config", "/app/proxy_server_config.yaml", "--port", "4000"]
    restart: unless-stopped
    env_file:
      - litellm/.env
    volumes:
      - ./litellm/config.yaml:/app/proxy_server_config.yaml:ro,Z
    networks:
      - model-net
    healthcheck:
      test: ["CMD", "python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:4000/health/liveliness', timeout=5)"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 60s
    security_opt:
      - no-new-privileges:true
      - label=disable
    cap_drop: [ALL]
    deploy:
      resources:
        limits:
          cpus: "1.0"
          memory: 1g

networks:
  agent-net: {}
  model-net: {}

volumes:
  agent-data:
  agent-backups:
`))

var sharedTemplate = template.Must(template.New("shared").Parse(`# RENDERED by agentctl from fleet.yaml — DO NOT EDIT.
# The envelope (pinned project name, volumes, networks, hardening) is
# owned by the fleet renderer; changes belong in fleet.yaml or the
# renderer, not here. Regenerated on every fleet verb.

# Pinned compose project name: every generated resource (containers,
# volumes, the agent-net network) is prefixed with it, so the stack is
# immune to directory renames and cannot collide with another agent on
# the same host. Real volume names are <project>_agent-data/-backups.
name: {{.Project}}

services:
  agent:
    build:
      context: .
      dockerfile: Dockerfile
    restart: unless-stopped
    # Keep the engine stop timeout above AGENT_SHUTDOWN_GRACE (600s
    # default) or the engine SIGKILLs the gateway process group
    # mid-drain.
    stop_grace_period: 11m
    # Hardening baseline matches the sidecar variant; see that render
    # for the rationale (rootless-podman label confinement off,
    # read-only root, tmpfs /tmp).
    security_opt:
      - no-new-privileges:true
      - label=disable
    cap_drop: [ALL]
    read_only: true
    tmpfs:
      - /tmp
    env_file:
      - .env
    # agent-net stays dedicated to the gateway port. The plane network
    # carries the shared LiteLLM proxy (and metrics scraping): the
    # per-agent alias is how siblings and the plane reach THIS agent —
    # every project names its service "agent", so the alias is the only
    # collision-free address.
    networks:
      agent-net: {}
      {{.PlaneNetwork}}:
        aliases:
          - {{.Project}}
    volumes:
      - agent-data:/home/node/.openclaw
      # Verified upgrade backups land here before any image-version
      # delta mutates a warm volume.
      - agent-backups:/backups
{{- range .Volumes }}
      - {{ . }}
{{- end }}
    # Loopback-only bind; remote access goes through a TLS-terminating
    # reverse proxy. AGENT_GATEWAY_PORT is host-side compose
    # interpolation and never enters the container; the fleet manifest
    # owns it (an AGENT_GATEWAY_PORT line in .env is an error).
    ports:
      - "127.0.0.1:${AGENT_GATEWAY_PORT:-{{.GatewayPort}}}:18789"
{{- range .Ports }}
      - "{{ . }}"
{{- end }}
    healthcheck:
      test: ["CMD", "node", "-e", "fetch('http://localhost:18789/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"]
      interval: 30s
      timeout: 10s
      retries: 3
      # First boot runs setup + reconcile + seed before the gateway
      # starts; upgrade boots add a verified backup.
      start_period: 300s
    deploy:
      resources:
        limits:
          cpus: "{{.Cpus}}"
          memory: {{.Memory}}
          pids: {{.Pids}}
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "5"

networks:
  agent-net: {}
  {{.PlaneNetwork}}:
    name: {{.PlaneNetwork}}
    external: true

volumes:
  agent-data:
  agent-backups:
`))

// RenderAgentCompose produces the deterministic deployment envelope
// for one roster agent: manifest facts (project name, allocated port,
// litellm placement, overrides) — never filesystem state.
func RenderAgentCompose(m *Manifest, name string) ([]byte, error) {
	entry, ok := m.Agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %q is not registered in %s", name, m.Path)
	}
	if entry.ComposeFile != "" {
		return nil, fmt.Errorf("agent %q: %w (%s)", name, ErrAuthoredCompose, entry.ComposeFile)
	}
	model := renderModel{
		Project:      entry.Name,
		GatewayPort:  entry.GatewayPort,
		Shared:       entry.LiteLLMUsed == LiteLLMShared,
		Volumes:      []string{},
		Ports:        []string{},
		Cpus:         "2.0",
		Memory:       "2g",
		Pids:         "512",
		LitellmImage: litellmSidecarImage,
	}
	if model.Shared {
		model.PlaneNetwork = PlaneNetworkName(m.Plane.Name)
	}
	if entry.Overrides != nil {
		model.Volumes = entry.Overrides.Volumes
		model.Ports = entry.Overrides.Ports
		if entry.Overrides.Limits != nil {
			model.Cpus = entry.Overrides.Limits.Cpus
			model.Memory = entry.Overrides.Limits.Memory
			model.Pids = fmt.Sprintf("%d", entry.Overrides.Limits.Pids)
		}
	}
	// Empty append-lists still render their section headers correctly
	// because the templates range over nil-safe slices; normalize nil
	// to empty for the model invariants.
	if model.Volumes == nil {
		model.Volumes = []string{}
	}
	if model.Ports == nil {
		model.Ports = []string{}
	}
	tmpl := sidecarTemplate
	if model.Shared {
		tmpl = sharedTemplate
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, model); err != nil {
		return nil, fmt.Errorf("rendering compose for agent %q: %w", name, err)
	}
	// text/template emits OS-flavored newlines only if the source had
	// them; normalize so goldens and materialized files stay stable.
	return []byte(strings.ReplaceAll(buf.String(), "\r\n", "\n")), nil
}

// MaterializeAgentCompose renders and writes the agent's envelope to
// <agent dir>/compose.yml (0o644), creating nothing else. It is the
// verb-time entry point: every fleet verb calls it before touching
// compose, so the artifact can never go stale while agentctl drives.
func MaterializeAgentCompose(m *Manifest, name string) (string, error) {
	data, err := RenderAgentCompose(m, name)
	if err != nil {
		return "", err
	}
	entry := m.Agents[name]
	if err := os.MkdirAll(entry.Dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", entry.Dir, err)
	}
	path := filepath.Join(entry.Dir, RenderedComposeName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}
