package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reportPath(t) gives a report destination inside a fresh dir and
// fails the test if any temp sibling leaks (atomic-write receipt).
func reportPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("leaked temp report file: %s", e.Name())
			}
		}
	})
	return filepath.Join(dir, "doctor-report.json")
}

func fileHex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestDoctorReport pins the D10 bundle: the standard document plus
// independently verifiable fingerprints, probe evidence when the
// instance answers, env KEY names only, an atomic write (no temp
// leftovers), and the path notice on text output.
func TestDoctorReport(t *testing.T) {
	root := fixtureProject(t)
	path := reportPath(t)
	// Secrets canary (mirrors the image-side SecretsCanary): a planted
	// value whose KEY must ship and whose VALUE must not.
	const canary = "canary-value-9f2c1"
	envPath := filepath.Join(agentDir(root), ".env")
	if err := os.WriteFile(envPath, []byte("FALLBACK_MODEL=m\nZAI_API_KEY=k\nSECRET_CANARY="+canary+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := stubbedRunner(t, "podman")
	r.runOutputs = map[string]string{
		postProbeKey(probeMarker): "2026.09.05\n",
		postProbeKey(probeStatus): `{"imageVersion":"2026.09.05","warnings":1,"bootCompletedAt":"2026-09-13T00:00:00Z"}`,
	}
	out, err := execIn(t, root, "doctor", "--report", path)
	if err != nil {
		t.Fatalf("doctor --report: %v\n%s", err, out)
	}
	if !strings.Contains(out, "report written to "+path) {
		t.Errorf("text output lacks the report path notice:\n%s", out)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report file: %v", err)
	}
	var doc doctorBundle
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, raw)
	}
	if doc.Failed {
		t.Error("bundle failed = true on a passing run")
	}
	if doc.Meta.Tag != "2026.09.05" {
		t.Errorf("meta.tag = %q, want 2026.09.05", doc.Meta.Tag)
	}
	if want := fileHex(t, filepath.Join(agentDir(root), "spec.json")); doc.Bundle.SpecSha256 != want {
		t.Errorf("spec_sha256 = %s, want %s", doc.Bundle.SpecSha256, want)
	}
	if want := fileHex(t, filepath.Join(agentDir(root), "compose.yml")); doc.Bundle.ComposeSha256 != want {
		t.Errorf("compose_sha256 = %s, want %s", doc.Bundle.ComposeSha256, want)
	}
	if doc.Bundle.LastImageVersion != "2026.09.05" {
		t.Errorf("last_image_version = %q, want 2026.09.05", doc.Bundle.LastImageVersion)
	}
	if doc.Bundle.StatusSummary == nil || doc.Bundle.StatusSummary.ImageVersion != "2026.09.05" || doc.Bundle.StatusSummary.Warnings != 1 {
		t.Errorf("status_summary = %+v, want image 2026.09.05 with 1 warning", doc.Bundle.StatusSummary)
	}
	for _, want := range []string{"FALLBACK_MODEL", "SECRET_CANARY", "ZAI_API_KEY"} {
		found := false
		for _, k := range doc.Bundle.EnvKeys {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("env_keys lacks %q: %v", want, doc.Bundle.EnvKeys)
		}
	}

	// The planted value must appear in neither the report file nor
	// stdout — only its key name ships.
	if strings.Contains(string(raw), canary) {
		t.Error("report file leaks the planted secret value")
	}
	if strings.Contains(out, canary) {
		t.Error("doctor stdout leaks the planted secret value")
	}
}

// TestDoctorReportJsonPureStdout keeps --json output machine-parseable
// when --report is set: the notice never lands on stdout.
func TestDoctorReportJsonPureStdout(t *testing.T) {
	root := fixtureProject(t)
	path := reportPath(t)
	stubbedRunner(t, "podman")
	out, err := execIn(t, root, "doctor", "--json", "--report", path)
	if err != nil {
		t.Fatalf("doctor --json --report: %v\n%s", err, out)
	}
	var doc doctorReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, out)
	}
	if strings.Contains(out, "report written to") {
		t.Error("path notice corrupted the JSON stdout stream")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("report file missing: %v", err)
	}
}

// TestDoctorReportUnreachableInstanceOmitsProbeFields proves the
// best-effort contract: without probe answers the bundle still ships,
// minus the instance evidence.
func TestDoctorReportUnreachableInstanceOmitsProbeFields(t *testing.T) {
	root := fixtureProject(t)
	path := reportPath(t)
	stubbedRunner(t, "podman") // no runOutputs: probes error
	out, err := execIn(t, root, "doctor", "--report", path)
	if err != nil {
		t.Fatalf("doctor --report: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc doctorBundle
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Bundle.LastImageVersion != "" || doc.Bundle.StatusSummary != nil {
		t.Errorf("unreachable instance must omit probe fields, got %+v", doc.Bundle)
	}
	if doc.Bundle.SpecSha256 == "" || len(doc.Bundle.EnvKeys) == 0 {
		t.Errorf("host-side evidence must still ship, got %+v", doc.Bundle)
	}
}

// TestDoctorReportFlagMisuse pins the mode exclusions.
func TestDoctorReportFlagMisuse(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	for _, args := range [][]string{
		{"doctor", "--report", "/tmp/x.json", "--target", "2026.09.12"},
		{"doctor", "--report", "/tmp/x.json", "--post-upgrade"},
	} {
		if _, err := execIn(t, root, args...); err == nil {
			t.Errorf("%v: expected a flag-misuse error", args)
		}
	}
}
