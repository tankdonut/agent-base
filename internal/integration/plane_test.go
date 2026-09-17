//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/e2e"
	"github.com/tankdonut/agent-base/internal/fleet"
)

// TestPlaneBootLitellmHealthy is the parked wedge's bounded diagnosis:
// materialize the plane, `compose up` under containment (240s budget,
// group kill + artifact dump on breach), then wait for the shared
// proxy's liveness on the loopback publish. A hang here now names
// itself and leaves compose ps + logs behind instead of stalling the
// suite.
func TestPlaneBootLitellmHealthy(t *testing.T) {
	engine := pickEngine(t)
	port := freePort(t)
	root := t.TempDir()
	manifest := fmt.Sprintf(`plane:
  enabled: true
  name: e2e-plane
  gateway_base_port: %d
  litellm: shared
  observability: false
agents:
  grow: {}
`, port)
	if err := os.WriteFile(filepath.Join(root, "fleet.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(root, "agents", "grow")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "spec.json"), []byte(`{"agent":{"name":"grow"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := fleet.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	// The authored secrets exist BEFORE materialize: the guard refuses
	// to render a plane whose .env is missing, and materialize never
	// overwrites the authored file.
	planeEnv := filepath.Join(root, fleet.PlaneDir, ".env")
	planePort := freePort(t)
	if err := os.MkdirAll(filepath.Dir(planeEnv), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planeEnv, []byte(fmt.Sprintf(
		"LITELLM_MASTER_KEY=sk-integration-master\nPOSTGRES_PASSWORD=integration-db\nPLANE_LITELLM_PORT=%d\n", planePort)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.MaterializePlane(m); err != nil {
		t.Fatalf("render plane: %v", err)
	}

	planeDir := filepath.Join(root, fleet.PlaneDir)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		_ = e2e.Run(ctx, e2e.Step{
			Name: "plane-down", Dir: planeDir,
			Argv: []string{engine, "compose", "-f", "compose.yml", "down", "--volumes"},
		})
	})

	// THE wedge call: compose up against the litellm-db plane stack,
	// contained. Pre-pull the plane's images first — the original
	// wedge was podman-compose pulling multi-GB layers inside `up`,
	// blowing any sane budget and dying mid-download.
	planeCompose := filepath.Join(planeDir, "compose.yml")
	for _, ref := range planeImages(t, planeCompose) {
		pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		if err := e2e.Run(pullCtx, e2e.Step{
			Name: "plane-pull", Dir: planeDir,
			Argv:   []string{engine, "pull", ref},
			Budget: 9 * time.Minute,
		}); err != nil {
			pullCancel()
			t.Fatalf("pre-pull %s: %v", ref, err)
		}
		pullCancel()
	}

	// THE wedge call: compose up against the litellm-db plane stack,
	// contained. Breach = fail fast with compose ps + logs dumped.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	upErr := e2e.Run(ctx, e2e.Step{
		Name: "plane-up", Dir: planeDir,
		Argv:    []string{engine, "compose", "-f", "compose.yml", "up", "-d"},
		Budget:  420 * time.Second,
		Dumpers: e2eDumper(engine, planeDir, "e2e-plane"),
	})
	if upErr != nil {
		t.Fatalf("plane up (contained): %v", upErr)
	}

	// Liveness: the proxy answers /health/liveliness on the loopback
	// publish within 150s (first boot runs the DB schema migration).
	deadline := time.Now().Add(150 * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/health/liveliness", planePort)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			// Pull the containers' last words before failing.
			dump := composeLogs(context.Background(), engine, planeDir)
			t.Fatalf("plane litellm never reached healthy on %s\n%s", url, dump)
		}
		time.Sleep(3 * time.Second)
	}
}

// planeImages extracts every `image:` ref from the rendered plane
// compose so the test can pre-pull them — podman-compose pulling
// multi-GB layers inside `up` was the original wedge.
func planeImages(t *testing.T, composePath string) []string {
	t.Helper()
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}
		ref := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
		if ref != "" {
			seen = append(seen, ref)
		}
	}
	if len(seen) == 0 {
		t.Fatal("rendered plane compose declares no images")
	}
	return seen
}

func e2eDumper(engine, planeDir, project string) []func(string) error {
	return []func(string) error{
		func(dir string) error {
			data, err := compose.PsJSON(newRunner(), engine, planeDir)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "compose-ps.json"), data, 0o644)
		},
		func(dir string) error {
			logs := composeLogs(context.Background(), engine, planeDir)
			return os.WriteFile(filepath.Join(dir, "compose-logs.txt"), []byte(logs), 0o644)
		},
	}
}

func composeLogs(ctx context.Context, engine, planeDir string) string {
	argv := []string{engine, "compose", "-f", "compose.yml", "logs", "--tail", "100"}
	cmd := commandIn(ctx, planeDir, argv)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("logs failed: %v\n%s", err, out)
	}
	return string(out)
}
