package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const planeLiteManifest = `plane:
  enabled: true
  name: e2e-plane
  observability: false
defaults:
  compose:
    engine: podman
agents:
  grow: {}
`

const planeFullManifest = `plane:
  enabled: true
  name: e2e-plane
  observability: true
defaults:
  compose:
    engine: podman
agents:
  grow: {}
  trade:
    litellm: sidecar
`

func planeFilesOrDie(t *testing.T, body string) map[string][]byte {
	t.Helper()
	m, err := LoadManifest(writeManifest(t, body))
	if err != nil {
		t.Fatal(err)
	}
	files, err := RenderPlaneFiles(m)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRenderPlaneLiteGolden(t *testing.T) {
	files := planeFilesOrDie(t, planeLiteManifest)
	checkGolden(t, "plane-lite.yml", string(files["compose.yml"]))
	if _, ok := files[".env.example"]; !ok {
		t.Error("shared plane must render .env.example")
	}
	if _, ok := files["observability/prometheus.yml"]; ok {
		t.Error("observability=false must not render prometheus config")
	}
}

func TestRenderPlaneFullGolden(t *testing.T) {
	files := planeFilesOrDie(t, planeFullManifest)
	compose := string(files["compose.yml"])
	checkGolden(t, "plane-full.yml", compose)
	checkGolden(t, "plane-prometheus.yml", string(files["observability/prometheus.yml"]))

	// The podman engine pin flows into the socket interpolation
	// default; rootless operators override PLANE_ENGINE_SOCK.
	if !strings.Contains(compose, "${PLANE_ENGINE_SOCK:-/run/podman/podman.sock}:/var/run/engine.sock:ro,Z") {
		t.Error("alloy socket mount must default to the podman system socket under defaults.compose.engine: podman")
	}
	if !strings.Contains(string(files["observability/config.alloy"]), "unix:///var/run/engine.sock") {
		t.Error("alloy config must address the fixed in-container socket path")
	}

	// Scrape targets are the SHARED roster only — the sidecar agent is
	// not on the plane network.
	prom := string(files["observability/prometheus.yml"])
	if !strings.Contains(prom, "grow:18789") || strings.Contains(prom, "trade:18789") {
		t.Errorf("prometheus targets wrong:\n%s", prom)
	}

	// Rendered artifacts never carry secret VALUES: everything secret
	// reaches compose as a ${VAR} interpolation name.
	canaryFiles := []string{compose, prom}
	for _, body := range canaryFiles {
		if strings.Contains(body, "sk-") || strings.Contains(body, "GENERATE_ME") {
			t.Error("rendered artifact embeds a secret-shaped value")
		}
	}
}

func TestRenderPlaneEngineSockDefault(t *testing.T) {
	files := planeFilesOrDie(t, strings.Replace(planeFullManifest, "engine: podman", "engine: docker", 1))
	if !strings.Contains(string(files["compose.yml"]), "${PLANE_ENGINE_SOCK:-/var/run/docker.sock}") {
		t.Error("docker engine must default the socket mount to /var/run/docker.sock")
	}
}

func TestRenderPlaneDisabledEmpty(t *testing.T) {
	files := planeFilesOrDie(t, "agents:\n  grow: {}\n")
	if len(files) != 0 {
		t.Errorf("disabled plane must render nothing, got %v", files)
	}
}

func TestRenderPlaneNoSharedNoDB(t *testing.T) {
	files := planeFilesOrDie(t, "plane:\n  enabled: true\n  name: p\n  litellm: none\n  observability: false\nagents:\n  grow: {}\n")
	compose := string(files["compose.yml"])
	if strings.Contains(compose, "litellm-db") || strings.Contains(compose, "litellm:") {
		t.Errorf("litellm: none must not render the proxy or DB:\n%s", compose)
	}
	if _, ok := files[".env.example"]; ok {
		t.Error("no shared proxy means no plane env example")
	}
}

func TestMaterializePlaneWritesAllAndGuardsEnv(t *testing.T) {
	root := writeManifest(t, planeFullManifest)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	// No plane/.env yet: files land, but the verb-time call reports the
	// missing authored surface.
	if _, err := MaterializePlane(m); err == nil || !strings.Contains(err.Error(), "no .env") {
		t.Fatalf("missing plane/.env must be reported, got %v", err)
	}
	for _, rel := range []string{
		"compose.yml",
		".env.example",
		"litellm/config.yaml",
		"observability/prometheus.yml",
		"observability/config.alloy",
		"observability/loki-config.yml",
		"observability/grafana/provisioning/datasources/prometheus.yml",
		"observability/grafana/provisioning/datasources/loki.yml",
	} {
		if _, err := os.Stat(filepath.Join(root, PlaneDir, rel)); err != nil {
			t.Errorf("plane/%s not materialized: %v", rel, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, PlaneDir, ".env"), []byte("LITELLM_MASTER_KEY=sk-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializePlane(m); err != nil {
		t.Fatalf("with plane/.env present materialize must succeed: %v", err)
	}
	// .env is authored state: regeneration must not touch it.
	before, _ := os.ReadFile(filepath.Join(root, PlaneDir, ".env"))
	if string(before) != "LITELLM_MASTER_KEY=sk-x\n" {
		t.Error("materialize overwrote the authored plane/.env")
	}
}
