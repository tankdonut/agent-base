// report.go builds the doctor --report bundle: the standard check
// document plus the attachable evidence D10 allows — file
// fingerprints, instance markers via read-only probes, and env KEY
// NAMES only. Secret values never enter the bundle by construction:
// agent/.env is read only for its key names, and probe output is
// limited to the version/status fields (SecretsCanary semantics).
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/project"
)

// reportStatus mirrors the image's {data}/status.json boot summary.
type reportStatus struct {
	ImageVersion string `json:"imageVersion,omitempty"`
	Warnings     int    `json:"warnings"`
}

// reportBundle is the evidence section of a --report file. Every
// field omits itself when its source is unavailable — a half-evidence
// report still ships.
type reportBundle struct {
	SpecSha256       string        `json:"spec_sha256,omitempty"`
	ComposeSha256    string        `json:"compose_sha256,omitempty"`
	LastImageVersion string        `json:"last_image_version,omitempty"`
	StatusSummary    *reportStatus `json:"status_summary,omitempty"`
	EnvKeys          []string      `json:"env_keys,omitempty"`
}

// doctorBundle is the --report file: the --json document plus the
// evidence bundle.
type doctorBundle struct {
	Meta   doctorMeta    `json:"meta"`
	Checks []CheckResult `json:"checks"`
	Failed bool          `json:"failed"`
	Bundle reportBundle  `json:"bundle"`
}

// buildReportBundle collects the evidence; each field is best-effort
// on its own (missing file or unreachable instance omits the field,
// never fails the report).
func buildReportBundle(ctx context.Context, plat platform.Platform, deploy *platform.Deployment, root string, info project.SpecInfo) reportBundle {
	var b reportBundle
	if sum, err := fileSha256(filepath.Join(root, "agent", "spec.json")); err == nil {
		b.SpecSha256 = sum
	}
	if sum, err := fileSha256(filepath.Join(root, "compose.yml")); err == nil {
		b.ComposeSha256 = sum
	}
	if plat != nil && deploy != nil && plat.Capabilities().Exec {
		if raw, err := plat.Probe(ctx, newRunner(), root, deploy, probeMarker); err == nil {
			if v := strings.TrimSpace(raw); v != "" {
				b.LastImageVersion = v
			}
		}
		if raw, err := plat.Probe(ctx, newRunner(), root, deploy, probeStatus); err == nil && strings.TrimSpace(raw) != "" {
			var st reportStatus
			if json.Unmarshal([]byte(raw), &st) == nil {
				b.StatusSummary = &st
			}
		}
	}
	b.EnvKeys = envKeyNames(root, info)
	return b
}

// envKeyNames unions the keys agent/.env defines with every name the
// spec can require. Names only — no .env value is ever read here.
func envKeyNames(root string, info project.SpecInfo) []string {
	set := map[string]bool{}
	if names, err := project.EnvKeyNames(root); err == nil {
		for _, n := range names {
			set[n] = true
		}
	}
	for _, n := range info.EnvRefs {
		set[n] = true
	}
	for _, n := range info.IfEnvNames {
		set[n] = true
	}
	if k := info.AuthEnvKey(); k != "" {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func fileSha256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// writeReportFile writes the bundle atomically: a temp file in the
// target directory, then rename — a reader never observes a torn
// report.
func writeReportFile(path string, doc doctorBundle) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating a temp report file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("moving the report into place: %w", err)
	}
	return nil
}
