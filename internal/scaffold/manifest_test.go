package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// wantContractFiles is the D9 drift scope: scaffold files whose shape
// is contract-load-bearing. spec.json and AGENTS.md are deliberately
// absent (operator-owned surfaces).
var wantContractFiles = []string{
	".env.example",
	"litellm/.env.example",
	"litellm/config.yaml",
}

func TestContractFiles(t *testing.T) {
	got := ContractFiles()
	if !reflect.DeepEqual(got, wantContractFiles) {
		t.Fatalf("ContractFiles() = %v, want %v", got, wantContractFiles)
	}
	paths, err := Paths()
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	generated := map[string]bool{}
	for _, p := range paths {
		generated[p] = true
	}
	for _, rel := range got {
		if !generated[rel] {
			t.Errorf("contract file %q is not a scaffold output path", rel)
		}
	}
}

// TestManifest proves the fingerprints are derived, not hand-maintained:
// every entry equals the sha256 of the live embedded template source.
func TestManifest(t *testing.T) {
	got, err := Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if !reflect.DeepEqual(gotKeys(got), wantContractFiles) {
		t.Fatalf("Manifest() keys = %v, want %v", gotKeys(got), wantContractFiles)
	}
	for _, rel := range wantContractFiles {
		src, err := fs.ReadFile(FS(), "tmpl/"+rel+".tmpl")
		if err != nil {
			t.Fatalf("read template %s: %v", rel, err)
		}
		sum := sha256.Sum256(src)
		if want := hex.EncodeToString(sum[:]); got[rel] != want {
			t.Errorf("Manifest[%q] = %s, want sha256 of the template source (%s)", rel, got[rel], want)
		}
	}
}

func gotKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// contractDataFor maps a full render Config onto the contract surface.
func contractDataFor(d data) ContractData {
	return ContractData{
		ProjectName:    d.ProjectName,
		ComposeProject: d.ComposeProject,
		AgentName:      d.AgentName,
		GatewayPort:    d.GatewayPort,
		Telegram:       d.Telegram,
	}
}

// TestRenderContractFileMatchesRun is the single-render-source proof:
// RenderContractFile must return byte-identical output to what Run
// writes for the same values, for both telegram shapes.
func TestRenderContractFileMatchesRun(t *testing.T) {
	for _, telegram := range []bool{true, false} {
		t.Run(map[bool]string{true: "telegram", false: "no-telegram"}[telegram], func(t *testing.T) {
			d := sampleData(telegram)
			dir := t.TempDir()
			cfg := Config{
				ProjectName: d.ProjectName,
				AgentName:   d.AgentName,
				BaseTag:     d.BaseTag,
				Model:       d.Model,
				GatewayPort: d.GatewayPort,
				Telegram:    d.Telegram,
				TargetDir:   dir,
			}
			if _, err := Run(cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, rel := range ContractFiles() {
				want, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(OutputPath(rel, ComposeProject(d.ProjectName)))))
				if err != nil {
					t.Fatalf("read scaffolded %s: %v", rel, err)
				}
				got, err := RenderContractFile(rel, contractDataFor(d))
				if err != nil {
					t.Fatalf("RenderContractFile(%q): %v", rel, err)
				}
				if string(got) != string(want) {
					t.Errorf("%s: RenderContractFile output differs from Run output\n--- render ---\n%s\n--- run ---\n%s", rel, got, want)
				}
				if strings.Contains(string(got), "{{") {
					t.Errorf("%s: output contains an unrendered template marker", rel)
				}
			}
		})
	}
}

// TestRenderContractFileRejectsNonContract fails closed on paths
// outside the drift scope (no accidental scope creep from callers).
func TestRenderContractFileRejectsNonContract(t *testing.T) {
	if _, err := RenderContractFile("agent/spec.json", contractDataFor(sampleData(true))); err == nil {
		t.Fatal("RenderContractFile accepted a non-contract path")
	}
}
