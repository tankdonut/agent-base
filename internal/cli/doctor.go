package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

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
	var target string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check everything agentctl needs before an issue can be filed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.OutOrStdout(), asJSON, target)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON (machine-readable)")
	cmd.Flags().StringVar(&target, "target", "", "preview an upgrade to this image tag (YYYY.MM.DD[.N]): era crossings + spec gate against the target")
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
// present only for what resolved before the checks ran; target is set
// only in --target previews.
type doctorMeta struct {
	Tag      string `json:"tag,omitempty"`
	Target   string `json:"target,omitempty"`
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

func runDoctor(out io.Writer, asJSON bool, target string) error {
	if target != "" && tagDate(target).IsZero() {
		return fmt.Errorf("--target %q is not a valid image tag (want YYYY.MM.DD[.N])", target)
	}
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
	if target != "" {
		meta.Target = target
	}

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

	// plat/deploy stay nil until the platform construct succeeds — the
	// volume probes below gate on plat != nil.
	var plat platform.Platform
	var deploy platform.Deployment
	cfg, err := LoadConfig()
	if err != nil {
		add("config", StatusFail, "config: %v", err)
	} else {
		meta.Platform = cfg.Platform
		if d, err := platform.Derive(root); err != nil {
			add("project", StatusFail, "project derivation: %v", err)
		} else {
			deploy = d
			meta.Project = d.Project
			p, err := forPlatform(cfg.Platform, newRunner(), cfg.Namespaces)
			if err != nil {
				add("platform", StatusFail, "platform %q: %v", cfg.Platform, err)
			} else {
				plat = p
				if err := p.Check(root, &deploy); err != nil {
					add("platform", StatusFail, "platform %q manifest: %v", cfg.Platform, err)
				} else {
					add("platform", StatusOK, "platform %q manifest lints (project %s, %d env keys set)", cfg.Platform, deploy.Project, len(deploy.EnvKeys))
				}
			}
		}
	}

	// --target previews an upgrade: which era boundaries a boot on the
	// target tag crosses, each with its remedy. Downgrades are named as
	// such — their crossings are not modeled; rollback is restore-from-
	// backup, not tag-walking.
	if target != "" {
		if tagOK && target != tag && !tagAfter(target, tag) {
			add("target-era", StatusWarn, "target %s is older than the pinned %s — downgrade crossings are not modeled; rollback means restoring the latest verified backup", target, tag)
		}
		if crossed := eraCrossings(tag, target, &info); len(crossed) == 0 {
			add("target-era", StatusOK, "no era crossings %s → %s", tag, target)
		} else {
			for _, e := range crossed {
				name := "era/" + e.ID
				add(name, e.Severity, "%s: %s: %s — %s", name, e.Release, e.Summary, e.Action)
			}
		}

		// Backup readiness: the migration boot writes its verified
		// archive into /backups before any mutating phase — a compose
		// without a volume there loses the archive with the container.
		if carries, cerr := backupsMountDeclared(filepath.Join(root, "compose.yml")); cerr != nil {
			add("backups-mount", StatusWarn, "compose.yml unreadable — skipped the /backups mount check (%v)", cerr)
		} else if !carries {
			add("backups-mount", StatusWarn, "no volume mounted at /backups — a migration boot's verified archive would die with the container; re-emit compose.yml from the current scaffold template")
		} else {
			add("backups-mount", StatusOK, "named volume mounted at /backups — migration archives survive container replacement")
		}
		if plat != nil && plat.Capabilities().Exec {
			marker, merr := plat.Probe(context.Background(), newRunner(), root, &deploy, "cat /home/node/.openclaw/last-image-version")
			switch {
			case merr != nil:
				add("volume-state", StatusWarn, "instance not reachable — skipped the volume probes (start it and re-run for warmth detection)")
			default:
				if trimmed := strings.TrimSpace(marker); trimmed != "" {
					add("volume-state", StatusOK, "warm volume — last migration marker %s; the upgrade boot takes a verified backup before mutating", trimmed)
				} else if _, oerr := plat.Probe(context.Background(), newRunner(), root, &deploy, "test -f /home/node/.openclaw/openclaw.json"); oerr != nil {
					add("volume-state", StatusOK, "fresh volume — first boot runs setup; no migration involved")
				} else {
					add("volume-state", StatusWarn, "next boot is a migration boot — expect the verified-backup line before reconcile (docs/standard-agent.md \"Upgrades\")")
				}
				if df, derr := plat.Probe(context.Background(), newRunner(), root, &deploy, "df -h /backups"); derr == nil {
					if line := lastLine(df); line != "" {
						add("backups-space", StatusOK, "/backups: %s", line)
					}
				}
			}
		}
	}

	// The real-image spec gate: the same --validate-spec run `agentctl
	// validate` performs. Engine and pinned image are prerequisites, not
	// verdicts — their absence warns instead of failing, so doctor still
	// reports everything else on an engineless host. --target is the
	// exception: an explicit preview pulls the target image instead of
	// skipping, because gating the target is the whole point.
	engine, err := resolveComposeEngine()
	switch {
	case err != nil:
		add("spec-gate", StatusWarn, "no compose engine — skipped the real-image spec gate")
	case target != "":
		ref := "ghcr.io/tankdonut/agent-base:" + target
		pulled := false
		if _, ierr := newRunner().RunOutput(nil, engine, "image", "inspect", ref); ierr != nil {
			if perr := newRunner().Run(nil, engine, "image", "pull", ref); perr != nil {
				add("spec-gate", StatusFail, "target image %s not local and the pull failed: %v — pull it manually and re-run", ref, perr)
				break
			}
			pulled = true
		}
		if err := compose.ValidateRef(newRunner(), engine, root, ref); err != nil {
			add("spec-gate", StatusFail, "target spec gate: %v", err)
		} else if pulled {
			add("spec-gate", StatusOK, "target spec gate passed via %s (pulled %s)", engine, ref)
		} else {
			add("spec-gate", StatusOK, "target spec gate passed via %s against %s", engine, ref)
		}
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

	// Tag freshness is advisory: the registry is the only source for
	// "is a newer release out" — an offline host keeps a full report;
	// the check degrades to a warn and never fails.
	if target != "" && tagOK {
		newest, nerr := newestPublishedTag(context.Background())
		switch {
		case nerr != nil:
			add("tag-freshness", StatusWarn, "registry unreachable — skipped the freshness check (%v)", nerr)
		case newest == target:
			add("tag-freshness", StatusOK, "target %s is the newest published tag", target)
		case tagAfter(newest, target):
			add("tag-freshness", StatusWarn, "newer image %s is published (target %s) — check the release notes before pinning older", newest, target)
		default:
			add("tag-freshness", StatusWarn, "target %s is not published (newest is %s) — date tags only; there is no latest", target, newest)
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

// backupsMountDeclared reports whether compose.yml mounts any volume
// at /backups on the agent service — where a migration boot writes its
// verified archive before mutating {data}.
func backupsMountDeclared(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var manifest struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return false, err
	}
	svc, ok := manifest.Services["agent"]
	if !ok {
		return false, nil
	}
	for _, v := range svc.Volumes {
		if strings.HasSuffix(v, ":/backups") {
			return true, nil
		}
	}
	return false, nil
}

// lastLine returns the last non-empty line of command output — df -h
// /backups's mount row under its header.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
