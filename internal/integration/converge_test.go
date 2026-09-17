//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/e2e"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/platform/dockercompose"
)

// convergeFixture returns the manifest, adapter, and agent dir with
// the envelope materialized (the pre-deploy render every verb does).
func convergeFixture(t *testing.T) (*fleet.Manifest, platform.Platform, string, string) {
	t.Helper()
	engine := pickEngine(t)
	ref := ensureBaseImage(t, engine)
	port := freePort(t)
	m, dir, dockerfile := fixtureAgent(t, engine, "grow", port)
	writeDockerfile(t, dockerfile, ref)
	if _, err := fleet.MaterializeAgentCompose(m, "grow"); err != nil {
		t.Fatal(err)
	}
	adapter, err := dockercompose.New(newRunner(), map[string]any{"engine": engine, "gateway_port": port})
	if err != nil {
		t.Fatal(err)
	}
	return m, adapter, dir, engine
}

// cleanupStack tears the stack down contained — a wedged down is as
// real as a wedged up.
func cleanupStack(t *testing.T, engine, dir string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		_ = e2e.Run(ctx, e2e.Step{
			Name: "down-" + filepath.Base(dir), Dir: dir,
			Argv: []string{engine, "compose", "-f", "compose.yml", "down", "--volumes"},
		})
	})
}

func TestConvergeAgentReachesRunningState(t *testing.T) {
	m, adapter, dir, engine := convergeFixture(t)
	cleanupStack(t, engine, dir)
	entry := m.Agents["grow"]
	d, err := platform.Derive(dir)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	deployErr := adapter.Deploy(ctx, newRunner(), dir, &d, platform.DeployOptions{}, fileOutput{os.Stdout})
	if deployErr != nil {
		t.Fatalf("deploy: %v", deployErr)
	}

	// compose ps shows the agent service Up — the convergence proof.
	data, err := compose.PsJSON(newRunner(), engine, dir)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("ps json: %v", err)
	}
	foundAgent := false
	for _, row := range rows {
		// podman ps --format json names containers under "Names" as an
		// ARRAY; docker compose uses "Name" as a string.
		name := ""
		switch v := row["Names"].(type) {
		case string:
			name = v
		case []any:
			if len(v) > 0 {
				name, _ = v[0].(string)
			}
		}
		if name == "" {
			name, _ = row["Name"].(string)
		}
		if strings.Contains(strings.ToLower(name), "agent") {
			foundAgent = true
		}
	}
	if !foundAgent {
		t.Errorf("no agent service in ps output: %s", data)
	}

	// The manifest port is what's published (docker Publishers /
	// podman host_port).
	published := publishedHostPorts(data)
	if !published[entry.GatewayPort] {
		t.Errorf("published ports %v missing the manifest allocation %d", keysOf(published), entry.GatewayPort)
	}

	// Stop + start round-trip through the adapter.
	if err := adapter.Stop(ctx, newRunner(), dir, &d, fileOutput{os.Stdout}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := adapter.Start(ctx, newRunner(), dir, &d, fileOutput{os.Stdout}); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestRunningPortDriftFlagsAfterManifestEdit(t *testing.T) {
	engine := pickEngine(t)
	ref := ensureBaseImage(t, engine)
	port := freePort(t)
	m, dir, dockerfile := fixtureAgent(t, engine, "grow", port)
	writeDockerfile(t, dockerfile, ref)
	if _, err := fleet.MaterializeAgentCompose(m, "grow"); err != nil {
		t.Fatal(err)
	}
	cleanupStack(t, engine, dir)
	d, err := platform.Derive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapterDeploy(t, engine, dir, &d); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Move the manifest allocation out from under the live stack —
	// exactly what a stale operator edit looks like — then reload and
	// re-render. The RUNNING stack still publishes the old port; the
	// fresh manifest allocates the new one. That mismatch IS the drift.
	old := port
	fleetPath := filepath.Join(filepath.Dir(filepath.Dir(dir)), "fleet.yaml")
	body, err := os.ReadFile(fleetPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body),
		"gateway_port: "+itoa(old), "gateway_port: "+itoa(old+1), 1)
	if edited == string(body) {
		t.Fatal("manifest edit was a no-op")
	}
	if err := os.WriteFile(fleetPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	moved, err := fleet.LoadManifest(filepath.Dir(fleetPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.MaterializeAgentCompose(moved, "grow"); err != nil {
		t.Fatal(err)
	}
	fresh, err := compose.PsJSON(newRunner(), engine, dir)
	if err != nil {
		t.Fatal(err)
	}
	published := publishedHostPorts(fresh)
	if !published[old] {
		t.Errorf("running stack no longer publishes the old port %d: %v", old, keysOf(published))
	}
	if published[old+1] {
		t.Errorf("running stack picked up the edited allocation %d without a redeploy — drift undetectable", old+1)
	}
}

func adapterDeploy(t *testing.T, engine, dir string, d *platform.Deployment) error {
	t.Helper()
	adapter, err := dockercompose.New(newRunner(), map[string]any{"engine": engine})
	if err != nil {
		return err
	}
	return adapter.Deploy(context.Background(), newRunner(), dir, d, platform.DeployOptions{}, fileOutput{os.Stdout})
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// publishedHostPorts normalizes compose ps --format json across
// engines (docker Publishers[].PublishedPort; podman Ports[].host_port).
func publishedHostPorts(data []byte) map[int]bool {
	ports := map[int]bool{}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return ports
	}
	for _, row := range rows {
		if pubs, ok := row["Publishers"].([]any); ok {
			for _, p := range pubs {
				if m, ok := p.(map[string]any); ok {
					if n, ok := m["PublishedPort"].(float64); ok && n > 0 {
						ports[int(n)] = true
					}
				}
			}
		}
		if tis, ok := row["Ports"].([]any); ok {
			for _, p := range tis {
				if m, ok := p.(map[string]any); ok {
					if n, ok := m["host_port"].(float64); ok && n > 0 {
						ports[int(n)] = true
					}
				}
			}
		}
	}
	return ports
}

func keysOf(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
