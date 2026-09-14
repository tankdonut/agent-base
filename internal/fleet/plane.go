// plane.go renders the shared-services plane: the compose stack the
// manifest's plane block describes (postgres + shared LiteLLM when
// litellm: shared; prometheus/loki/alloy/grafana when observability),
// plus every auxiliary config those services mount. All files are
// derived state — materialized under plane/ on every plane verb and
// gitignored wholesale; the only authored surface is plane/.env.
package fleet

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

// PlaneDir is the plane's directory at the fleet root.
const PlaneDir = "plane"

// Plane image pins. The litellm pin mirrors the sidecar constant
// (bumped together at release). The observability pins are tags, not
// digests — the observability stack boots in e2e with P5, which pins
// them for real.
const (
	planePostgresImage = "postgres:17-alpine"
	planePromImage     = "prom/prometheus:v3.7.1"
	planeLokiImage     = "grafana/loki:3.5.2"
	planeAlloyImage    = "grafana/alloy:v1.9.2"
	planeGrafanaImage  = "grafana/grafana:12.1.0"
)

// planeModel carries the compose interpolations.
type planeModel struct {
	Name          string
	Net           string
	Shared        bool
	Observability bool
	// EngineSock is the HOST socket path interpolated as the compose
	// default: the docker socket, or podman's system-service socket
	// when defaults.compose.engine is pinned to podman. Rootless
	// podman operators override PLANE_ENGINE_SOCK in plane/.env — the
	// runtime UID is not knowable at render time. Inside the container
	// the socket always lives at /var/run/engine.sock, so the alloy
	// config stays engine-independent.
	EngineSock    string
	PromImage     string
	LokiImage     string
	AlloyImage    string
	GrafanaImage  string
	PostgresImage string
	LitellmImage  string
}

// EngineSocketDefault maps the manifest's compose engine to its
// well-known host socket path (docker's is universal; podman's is the
// system-service rootful socket).
func EngineSocketDefault(engine string) string {
	if engine == "podman" {
		return "/run/podman/podman.sock"
	}
	return "/var/run/docker.sock"
}

var planeComposeTemplate = template.Must(template.New("plane").Parse(`# RENDERED by agentctl from fleet.yaml — DO NOT EDIT.
# The plane (shared services) is derived state, regenerated on every
# fleet plane verb. Secrets live only in plane/.env (interpolation
# source AND litellm env_file); nothing rendered carries a value.

# Pinned project name: containers/volumes/network carry it as a prefix,
# and the network's explicit name is what agents join as external.
name: {{.Name}}

services:
{{- if .Shared }}
  # Budgets/virtual keys REQUIRE the database (db-less LiteLLM fails
  # budgets open) — the DB is not optional for the shared plane.
  litellm-db:
    image: {{.PostgresImage}}
    restart: unless-stopped
    environment:
      POSTGRES_USER: litellm
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_DB: litellm
    volumes:
      - plane-litellm-db:/var/lib/postgresql/data
    networks:
      - {{.Net}}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U litellm -d litellm"]
      interval: 10s
      timeout: 5s
      retries: 6
    security_opt:
      - no-new-privileges:true
      - label=disable
    cap_drop: [ALL]

  # The fleet's one model/provider home: every provider API key lives
  # in plane/.env, agents hold per-agent virtual keys minted by
  # 'fleet key <agent>' (budgets/teams in the litellm-db).
  litellm:
    image: {{.LitellmImage}}
    command: ["--config", "/app/proxy_server_config.yaml", "--port", "4000"]
    restart: unless-stopped
    env_file:
      - .env
    environment:
      DATABASE_URL: postgresql://litellm:${POSTGRES_PASSWORD}@litellm-db:5432/litellm
      STORE_MODEL_IN_DB: "True"
    volumes:
      - ./litellm/config.yaml:/app/proxy_server_config.yaml:ro,Z
    # Loopback-only: agents reach the proxy over {{.Net}} as
    # http://litellm:4000; this publish is for the operator and
    # 'fleet key' minting.
    ports:
      - "127.0.0.1:${PLANE_LITELLM_PORT:-4000}:4000"
    networks:
      - {{.Net}}
    depends_on:
      litellm-db:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:4000/health/liveliness', timeout=5)"]
      interval: 30s
      timeout: 10s
      retries: 5
      # First boot runs the prisma schema migration against postgres.
      start_period: 120s
    security_opt:
      - no-new-privileges:true
      - label=disable
    cap_drop: [ALL]
    deploy:
      resources:
        limits:
          cpus: "1.0"
          memory: 1g
{{- end }}
{{- if .Observability }}
  prometheus:
    image: {{.PromImage}}
    command:
      - --config.file=/etc/prometheus/prometheus.yml
      - --storage.tsdb.retention.time=15d
    volumes:
      - ./observability/prometheus.yml:/etc/prometheus/prometheus.yml:ro,Z
      - plane-prometheus:/prometheus
    networks:
      - {{.Net}}
    security_opt:
      - no-new-privileges:true
      - label=disable
    cap_drop: [ALL]

  loki:
    image: {{.LokiImage}}
    command: ["-config.file=/etc/loki/local-config.yaml"]
    volumes:
      - ./observability/loki-config.yml:/etc/loki/local-config.yaml:ro,Z
      - plane-loki:/loki
    networks:
      - {{.Net}}
    security_opt:
      - no-new-privileges:true
      - label=disable
    cap_drop: [ALL]

  # Log collection: docker_sd discovers running containers, relabeling
  # keeps the compose project as the Loki label (one stream namespace
  # per agent). The engine socket mounts read-only at a fixed
  # in-container path; the HOST side defaults per engine (docker vs
  # podman system service) and rootless podman sets PLANE_ENGINE_SOCK.
  alloy:
    image: {{.AlloyImage}}
    command: ["run", "/etc/alloy/config.alloy", "--server.http.listen-addr=0.0.0.0:12345"]
    volumes:
      - ./observability/config.alloy:/etc/alloy/config.alloy:ro,Z
      - ${PLANE_ENGINE_SOCK:-{{.EngineSock}}}:/var/run/engine.sock:ro,Z
    networks:
      - {{.Net}}
    security_opt:
      - no-new-privileges:true
      - label=disable

  grafana:
    image: {{.GrafanaImage}}
    restart: unless-stopped
    environment:
      GF_SECURITY_ADMIN_USER: admin
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_ADMIN_PASSWORD}
    volumes:
      - ./observability/grafana/provisioning:/etc/grafana/provisioning:ro,Z
      - plane-grafana:/var/lib/grafana
    ports:
      - "127.0.0.1:${PLANE_GRAFANA_PORT:-3000}:3000"
    networks:
      - {{.Net}}
    depends_on:
      - prometheus
      - loki
{{- end }}

networks:
  {{.Net}}:
    name: {{.Net}}

{{- if or .Shared .Observability }}

volumes:
{{- if .Shared }}
  plane-litellm-db:
{{- end }}
{{- if .Observability }}
  plane-prometheus:
  plane-loki:
  plane-grafana:
{{- end }}
{{- end }}
`))

var planeLitellmConfigTemplate = template.Must(template.New("plane-litellm").Parse(`# RENDERED by agentctl — LiteLLM proxy config for the shared plane.
# Unlike the db-less sidecar, this proxy runs WITH postgres
# (STORE_MODEL_IN_DB=True): models added through the proxy UI/API land
# in the database and survive re-renders — keep file additions minimal
# and prefer the DB for anything per-agent.
#
# model_list stays EMPTY by design (a fresh plane must boot without
# provider credentials); add entries the same way as the sidecar:
#
# model_list:
#   - model_name: glm-5.3
#     litellm_params:
#       model: zai/glm-5.3
#       api_key: os.environ/ZAI_API_KEY   # key lives in plane/.env
model_list: []
`))

var planeEnvExampleTemplate = template.Must(template.New("plane-env").Parse(`# RENDERED by agentctl — the plane's secret surface. Copy to .env,
# fill every GENERATE_ME, never commit (plane/ is gitignored).
# .env doubles as the compose interpolation source AND litellm's
# env_file: provider keys set here serve the whole fleet.
LITELLM_MASTER_KEY=sk-GENERATE_ME
POSTGRES_PASSWORD=GENERATE_ME
{{- if .Observability }}
GRAFANA_ADMIN_PASSWORD=GENERATE_ME
# Host path of the engine socket alloy reads (read-only). The default
# follows defaults.compose.engine (docker: /var/run/docker.sock,
# podman: /run/podman/podman.sock); rootless podman must set its own:
#PLANE_ENGINE_SOCK=/run/user/1000/podman/podman.sock
{{- end }}
# Provider keys (one per provider the fleet uses):
#ZAI_API_KEY=
`))

var planePrometheusTemplate = template.Must(template.New("plane-prom").Parse(`# RENDERED by agentctl — scrape targets are the shared-plane roster
# (per-agent aliases on {{.Net}}; sidecar agents are not reachable from
# the plane and are not scraped). Each agent must run the
# diagnostics-prometheus plugin (docs: docs/standard-agent.md) and —
# because that plugin requires operator auth — the job needs the
# agent's gateway token as a bearer credential; wiring tokens lands
# with the live-observability phase, which owns the scrape contract.
global:
  scrape_interval: 30s
scrape_configs:{{range .Agents}}
  - job_name: {{.}}
    static_configs:
      - targets: ["{{.}}:18789"]
{{end}}`))

var planeAlloyTemplate = template.Must(template.New("plane-alloy").Parse(`// RENDERED by agentctl — log collection for the whole host's agent
// containers: docker-compatible discovery + compose-project relabel,
// shipped to the plane's Loki. The engine socket (docker or podman) is
// mounted read-only at the fixed path /var/run/engine.sock, so this
// config stays engine-independent.
discovery.docker "containers" {
  host = "unix:///var/run/engine.sock"
}

discovery.relabel "agent_containers" {
  target = discovery.docker.containers.target

  rule {
    source_labels = ["__meta_docker_container_label_com_docker_compose_project"]
    regex         = ".+"
    action        = "keep"
  }
  rule {
    source_labels = ["__meta_docker_container_label_com_docker_compose_project"]
    target_label  = "compose_project"
  }
  rule {
    source_labels = ["__meta_docker_container_name"]
    regex         = "/(.*)"
    target_label  = "container"
  }
}

loki.write "plane" {
  endpoint {
    url = "http://loki:3100/loki/api/v1/push"
  }
}

loki.source.docker "agent_containers" {
  host       = "unix:///var/run/engine.sock"
  targets    = discovery.relabel.agent_containers.output
  forward_to = [loki.write.plane.receiver]
}
`))

var planeLokiConfigTemplate = template.Must(template.New("plane-loki").Parse(`# RENDERED by agentctl — single-binary filesystem mode (no object
# storage; the plane is a single host by design).
auth_enabled: false
server:
  http_listen_port: 3100
common:
  path_prefix: /loki
  storage:
    filesystem:
      chunks_directory: /loki/chunks
      rules_directory: /loki/rules
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory
schema_config:
  configs:
    - from: "2024-01-01"
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: index_
        period: 24h
`))

var planeGrafanaDatasourcePromTemplate = template.Must(template.New("gf-prom").Parse(`# RENDERED by agentctl
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
`))

var planeGrafanaDatasourceLokiTemplate = template.Must(template.New("gf-loki").Parse(`# RENDERED by agentctl
apiVersion: 1
datasources:
  - name: Loki
    type: loki
    access: proxy
    url: http://loki:3100
    jsonData:
      derivedFields:
        - name: compose_project
          matcherRegex: "compose_project=([^,]+)"
          url: "$${__value.raw}"
`))

// RenderPlaneFiles produces every plane artifact: path (relative to
// plane/) → content. Empty when the plane is disabled.
func RenderPlaneFiles(m *Manifest) (map[string][]byte, error) {
	if !m.Plane.Enabled {
		return map[string][]byte{}, nil
	}
	shared := m.Plane.LiteLLM == LiteLLMShared
	model := planeModel{
		Name:          m.Plane.Name,
		Net:           PlaneNetworkName(m.Plane.Name),
		Shared:        shared,
		Observability: m.Plane.Observability,
		EngineSock:    EngineSocketDefault(m.Defaults.ComposeEngine),
		PromImage:     planePromImage,
		LokiImage:     planeLokiImage,
		AlloyImage:    planeAlloyImage,
		GrafanaImage:  planeGrafanaImage,
		PostgresImage: planePostgresImage,
		LitellmImage:  litellmSidecarImage,
	}
	files := map[string][]byte{}
	if err := addRendered(files, "compose.yml", planeComposeTemplate, model); err != nil {
		return nil, err
	}
	if shared {
		if err := addRendered(files, ".env.example", planeEnvExampleTemplate, model); err != nil {
			return nil, err
		}
		if err := addRendered(files, "litellm/config.yaml", planeLitellmConfigTemplate, model); err != nil {
			return nil, err
		}
	}
	if model.Observability {
		promModel := struct {
			Net    string
			Agents []string
		}{Net: model.Net}
		for _, name := range m.AgentNames() {
			if m.Agents[name].LiteLLMUsed == LiteLLMShared {
				promModel.Agents = append(promModel.Agents, name)
			}
		}
		if err := addRendered(files, "observability/prometheus.yml", planePrometheusTemplate, promModel); err != nil {
			return nil, err
		}
		for rel, tmpl := range map[string]*template.Template{
			"observability/config.alloy":                                    planeAlloyTemplate,
			"observability/loki-config.yml":                                 planeLokiConfigTemplate,
			"observability/grafana/provisioning/datasources/prometheus.yml": planeGrafanaDatasourcePromTemplate,
			"observability/grafana/provisioning/datasources/loki.yml":       planeGrafanaDatasourceLokiTemplate,
		} {
			if err := addRendered(files, rel, tmpl, model); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

func addRendered(files map[string][]byte, rel string, tmpl *template.Template, data any) error {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("rendering plane/%s: %w", rel, err)
	}
	files[rel] = []byte(strings.ReplaceAll(buf.String(), "\r\n", "\n"))
	return nil
}

// MaterializePlane writes every rendered plane artifact under
// <root>/plane/, creating the directory. plane/.env is never written
// (authored surface); .env.example is regenerated on every call.
// Returns the written paths relative to the fleet root.
func MaterializePlane(m *Manifest) ([]string, error) {
	files, err := RenderPlaneFiles(m)
	if err != nil {
		return nil, err
	}
	if !m.Plane.Enabled {
		return nil, nil
	}
	base := filepath.Join(m.Root, PlaneDir)
	written := make([]string, 0, len(files))
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, files[rel], 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", path, err)
		}
		written = append(written, filepath.Join(PlaneDir, filepath.FromSlash(rel)))
	}
	if _, err := os.Stat(filepath.Join(base, ".env")); err != nil {
		return written, fmt.Errorf("%s carries no .env — copy %s to .env, fill every GENERATE_ME, then re-run", base, filepath.Join(PlaneDir, ".env.example"))
	}
	return written, nil
}
