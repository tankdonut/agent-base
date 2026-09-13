package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/project"
)

// newDoctorCmd is the one-command pre-issue report: project shape,
// spec, pinned base tag, secrets, platform manifest, engine, the
// litellm provider shape (with migration readiness for non-litellm
// providers), and the real-image spec gate — every check fail-closed
// with the fix named. Host-side twin of the image's doctor skills.
func newDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check everything agentctl needs before an issue can be filed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON (machine-readable)")
	return cmd
}

// CheckStatus is a doctor check verdict. Text doctor prints ok, FAIL,
// and warn lines; skip is reserved for checks that could not run at
// all and renders nothing today.
type CheckStatus string

const (
	StatusOK   CheckStatus = "ok"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
	StatusSkip CheckStatus = "skip"
)

// CheckResult is one doctor check. Detail is the exact report line —
// remedy included, em-dash separated, as doctor has always printed it.
// Fix is a separate remediation field for machine consumers, populated
// when checks grow structured fixes (--target crossings onward); the
// text renderer never reads it.
type CheckResult struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
	Fix    string      `json:"fix"`
}

// anyFailed reports whether any result is a FAIL.
func anyFailed(results []CheckResult) bool {
	for _, r := range results {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

// renderDoctor prints the check lines in collected order and the
// verdict — byte-identical to doctor's pre-model output. Skip results
// render nothing.
func renderDoctor(out io.Writer, results []CheckResult) error {
	for _, r := range results {
		switch r.Status {
		case StatusFail:
			fmt.Fprintf(out, "FAIL  %s\n", r.Detail)
		case StatusWarn:
			fmt.Fprintf(out, "warn  %s\n", r.Detail)
		case StatusSkip:
			// reserved: no skip line renders today
		default:
			fmt.Fprintf(out, "ok    %s\n", r.Detail)
		}
	}
	if anyFailed(results) {
		return fmt.Errorf("doctor found problems — fix the FAIL lines above")
	}
	fmt.Fprintln(out, "all checks passed")
	return nil
}

// doctorMeta carries the project identity the report ran against —
// present only for what resolved before the checks ran.
type doctorMeta struct {
	Tag      string `json:"tag,omitempty"`
	Platform string `json:"platform,omitempty"`
	Project  string `json:"project,omitempty"`
}

// doctorReport is the --json rendering of a doctor run: identity,
// every check, and the verdict. Exit semantics match text doctor — a
// FAIL verdict still exits non-zero with the same error.
type doctorReport struct {
	Meta   doctorMeta    `json:"meta"`
	Checks []CheckResult `json:"checks"`
	Failed bool          `json:"failed"`
}

func runDoctor(out io.Writer, asJSON bool) error {
	root, err := chdirProject()
	if err != nil {
		return err
	}
	var results []CheckResult
	add := func(name string, status CheckStatus, format string, a ...any) {
		results = append(results, CheckResult{
			Name:   name,
			Status: status,
			Detail: fmt.Sprintf(format, a...),
		})
	}
	meta := doctorMeta{}

	specOK := true
	info, err := project.ReadSpec(filepath.Join(root, "agent", "spec.json"))
	switch {
	case err != nil:
		add("spec", StatusFail, "spec.json: %v", err)
		specOK = false
	default:
		add("spec", StatusOK, "spec.json parses (%d env refs, %d if_env guards)", len(info.EnvRefs), len(info.IfEnvNames))
	}

	tagOK := true
	tag, err := project.BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
	switch {
	case err != nil:
		add("base-tag", StatusFail, "Dockerfile base tag: %v", err)
		tagOK = false
	default:
		meta.Tag = tag
		add("base-tag", StatusOK, "base image pinned: ghcr.io/tankdonut/agent-base:%s", tag)
	}

	if n, err := project.SecretsCheck(root); err != nil {
		add("secrets", StatusFail, "secrets: %v", err)
	} else {
		add("secrets", StatusOK, "agent/.env sets all %d required vars", n)
	}

	// The litellm provider shape is load-bearing for litellm specs: the
	// tree, the compose sidecar, and model-net must all be present — and
	// the pinned image must actually ship the litellm seed. For other
	// providers the report is advisory only — direct-provider
	// deployments remain supported.
	if specOK && info.AuthChoice == "litellm-api-key" {
		if seed, ok := eraByID("litellm-baseurl-seed"); ok {
			if day := tagDate(tag); tagOK && !day.IsZero() && day.Year() >= 2026 && day.Before(seed.Day) {
				add("litellm-era", StatusFail, "pinned base image %s predates the litellm seed (%s) — bump agent/Dockerfile; the old image never seeds baseUrl and its loader does not gate the key", tag, seed.Day.Format("2006.01.02"))
			}
		}
		if _, err := os.Stat(filepath.Join(root, "litellm", ".env.example")); err != nil {
			add("litellm-tree", StatusFail, "litellm/.env.example not found — adopt the litellm tree (docs/standard-agent.md \"Model providers via LiteLLM\")")
		} else if !composeCarriesLitellm(filepath.Join(root, "compose.yml")) {
			add("litellm-tree", StatusFail, "compose.yml lacks the litellm sidecar or model-net — re-emit it from the current scaffold template or add the service block")
		} else {
			add("litellm-tree", StatusOK, "litellm sidecar shape present (tree, compose service, model-net)")
		}
	} else if specOK {
		add("litellm-tree", StatusWarn, "provider %q — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md \"Migrating an existing agent to LiteLLM\")", info.AuthChoice)
	}

	cfg, err := LoadConfig()
	if err != nil {
		add("config", StatusFail, "config: %v", err)
	} else {
		meta.Platform = cfg.Platform
		d, err := platform.Derive(root)
		if err != nil {
			add("project", StatusFail, "project derivation: %v", err)
		} else {
			meta.Project = d.Project
			p, err := forPlatform(cfg.Platform, newRunner(), cfg.Namespaces)
			if err != nil {
				add("platform", StatusFail, "platform %q: %v", cfg.Platform, err)
			} else if err := p.Check(root, &d); err != nil {
				add("platform", StatusFail, "platform %q manifest: %v", cfg.Platform, err)
			} else {
				add("platform", StatusOK, "platform %q manifest lints (project %s, %d env keys set)", cfg.Platform, d.Project, len(d.EnvKeys))
			}
		}
	}

	// The real-image spec gate: the same --validate-spec run `agentctl
	// validate` performs. Engine and pinned image are prerequisites, not
	// verdicts — their absence warns instead of failing, so doctor still
	// reports everything else on an engineless host.
	engine, err := resolveComposeEngine()
	switch {
	case err != nil:
		add("spec-gate", StatusWarn, "no compose engine — skipped the real-image spec gate")
	case !tagOK:
		add("spec-gate", StatusWarn, "base tag unreadable — skipped the real-image spec gate")
	default:
		ref := "ghcr.io/tankdonut/agent-base:" + tag
		if _, err := newRunner().RunOutput(nil, engine, "image", "inspect", ref); err != nil {
			add("spec-gate", StatusWarn, "pinned image %s not local — skipped the real-image spec gate (run `agentctl deploy` once or pull it)", ref)
		} else if err := compose.Validate(newRunner(), engine, root); err != nil {
			add("spec-gate", StatusFail, "real-image spec gate: %v", err)
		} else {
			add("spec-gate", StatusOK, "real-image spec gate passed via %s", engine)
		}
	}

	if asJSON {
		report := doctorReport{Meta: meta, Checks: results, Failed: anyFailed(results)}
		data, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("marshal doctor report: %w", err)
		}
		fmt.Fprintln(out, string(data))
		if report.Failed {
			return fmt.Errorf("doctor found problems — fix the FAIL lines above")
		}
		return nil
	}
	return renderDoctor(out, results)
}

// tagDate parses the YYYY.MM.DD prefix of a base-image tag (an optional
// .N run suffix is ignored for ordering). Zero time on a malformed tag —
// callers treat that as "era unknown" and skip the check.
func tagDate(tag string) time.Time {
	parts := strings.SplitN(tag, ".", 4)
	if len(parts) < 3 {
		return time.Time{}
	}
	day, err := time.Parse("2006.01.02", strings.Join(parts[:3], "."))
	if err != nil {
		return time.Time{}
	}
	return day
}

// composeCarriesLitellm reports whether the compose file names the
// litellm service and the model-net network — the blessed shape the
// base seeds baseUrl against.
func composeCarriesLitellm(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	content := string(data)
	return strings.Contains(content, "litellm:") && strings.Contains(content, "model-net")
}
