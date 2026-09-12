package cli

import (
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
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check everything agentctl needs before an issue can be filed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.OutOrStdout())
		},
	}
}

func runDoctor(out io.Writer) error {
	root, err := chdirProject()
	if err != nil {
		return err
	}
	ok := func(format string, a ...any) { fmt.Fprintf(out, "ok    "+format+"\n", a...) }
	fail := func(format string, a ...any) { fmt.Fprintf(out, "FAIL  "+format+"\n", a...) }
	warn := func(format string, a ...any) { fmt.Fprintf(out, "warn  "+format+"\n", a...) }
	failed := false

	specOK := true
	info, err := project.ReadSpec(filepath.Join(root, "agent", "spec.json"))
	switch {
	case err != nil:
		fail("spec.json: %v", err)
		failed = true
		specOK = false
	default:
		ok("spec.json parses (%d env refs, %d if_env guards)", len(info.EnvRefs), len(info.IfEnvNames))
	}

	tagOK := true
	tag, err := project.BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
	switch {
	case err != nil:
		fail("Dockerfile base tag: %v", err)
		failed = true
		tagOK = false
	default:
		ok("base image pinned: ghcr.io/tankdonut/agent-base:%s", tag)
	}

	if n, err := project.SecretsCheck(root); err != nil {
		fail("secrets: %v", err)
		failed = true
	} else {
		ok("agent/.env sets all %d required vars", n)
	}

	// The litellm provider shape is load-bearing for litellm specs: the
	// tree, the compose sidecar, and model-net must all be present — and
	// the pinned image must actually ship the litellm seed. For other
	// providers the report is advisory only — direct-provider
	// deployments remain supported.
	if specOK && info.AuthChoice == "litellm-api-key" {
		if day := tagDate(tag); tagOK && !day.IsZero() && day.Year() >= 2026 && day.Before(litellmSeedDay) {
			fail("pinned base image %s predates the litellm seed (2026.09.12) — bump agent/Dockerfile; the old image never seeds baseUrl and its loader does not gate the key", tag)
			failed = true
		}
		if _, err := os.Stat(filepath.Join(root, "litellm", ".env.example")); err != nil {
			fail("litellm/.env.example not found — adopt the litellm tree (docs/standard-agent.md \"Model providers via LiteLLM\")")
			failed = true
		} else if !composeCarriesLitellm(filepath.Join(root, "compose.yml")) {
			fail("compose.yml lacks the litellm sidecar or model-net — re-emit it from the current scaffold template or add the service block")
			failed = true
		} else {
			ok("litellm sidecar shape present (tree, compose service, model-net)")
		}
	} else if specOK {
		warn("provider %q — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md \"Migrating an existing agent to LiteLLM\")", info.AuthChoice)
	}

	cfg, err := LoadConfig()
	if err != nil {
		fail("config: %v", err)
		failed = true
	} else {
		d, err := platform.Derive(root)
		if err != nil {
			fail("project derivation: %v", err)
			failed = true
		} else {
			p, err := forPlatform(cfg.Platform, newRunner(), cfg.Namespaces)
			if err != nil {
				fail("platform %q: %v", cfg.Platform, err)
				failed = true
			} else if err := p.Check(root, &d); err != nil {
				fail("platform %q manifest: %v", cfg.Platform, err)
				failed = true
			} else {
				ok("platform %q manifest lints (project %s, %d env keys set)", cfg.Platform, d.Project, len(d.EnvKeys))
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
		warn("no compose engine — skipped the real-image spec gate")
	case !tagOK:
		warn("base tag unreadable — skipped the real-image spec gate")
	default:
		ref := "ghcr.io/tankdonut/agent-base:" + tag
		if _, err := newRunner().RunOutput(nil, engine, "image", "inspect", ref); err != nil {
			warn("pinned image %s not local — skipped the real-image spec gate (run `agentctl deploy` once or pull it)", ref)
		} else if err := compose.Validate(newRunner(), engine, root); err != nil {
			fail("real-image spec gate: %v", err)
			failed = true
		} else {
			ok("real-image spec gate passed via %s", engine)
		}
	}

	if failed {
		return fmt.Errorf("doctor found problems — fix the FAIL lines above")
	}
	fmt.Fprintln(out, "all checks passed")
	return nil
}

// litellmSeedDay is the first release carrying the litellm loader gate
// and the baseUrl seed (2026.09.12). Litellm-shaped projects pinned to
// older images boot with openclaw's loopback provider default — an era
// mismatch doctor must name, not a shape problem.
var litellmSeedDay = time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)

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
