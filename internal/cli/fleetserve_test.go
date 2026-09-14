package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/fleet"
)

// upgradesPreviewFixture: two agents pinned at different base tags —
// one whose target upgrade crosses era entries, one already ahead.
func upgradesPreviewFixture(t *testing.T) *fleet.Manifest {
	t.Helper()
	root := makeFleetRepo(t, "agents:\n  grow: {}\n  trade: {}\n", []string{"grow", "trade"}, nil)
	pin := func(name, tag string) {
		path := filepath.Join(root, "agents", name, "Dockerfile")
		if err := os.WriteFile(path, []byte("FROM ghcr.io/tankdonut/agent-base:"+tag+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pin("grow", "2026.09.05")
	pin("trade", "2026.09.14")
	m, err := fleet.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpgradesPreviewWalksEraTable(t *testing.T) {
	m := upgradesPreviewFixture(t)

	preview, err := upgradesPreview(m, "grow", "2026.09.14")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(preview)
	var decoded struct {
		Agent      string `json:"agent"`
		CurrentTag string `json:"current_tag"`
		Target     string `json:"target"`
		Downgrade  bool   `json:"downgrade"`
		Crossings  []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"crossings"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Agent != "grow" || decoded.CurrentTag != "2026.09.05" || decoded.Target != "2026.09.14" {
		t.Errorf("preview header = %+v", decoded)
	}
	if decoded.Downgrade {
		t.Error("2026.09.05 → 2026.09.14 is not a downgrade")
	}

	// A downgrade flags without fabricating crossings.
	preview, err = upgradesPreview(m, "trade", "2026.09.05")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(preview)
	if !strings.Contains(string(body), `"downgrade":true`) {
		t.Errorf("downgrade preview = %s", body)
	}
}
