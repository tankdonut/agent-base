package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/lifecycle"
	"github.com/tankdonut/agent-base/internal/platform"
)

// newDoctorCmd is the one-command pre-issue report: project shape,
// spec, pinned base tag, secrets, platform manifest, and engine — every
// check fail-closed with the fix named. Host-side twin of the image's
// doctor skills.
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
	failed := false

	info, err := lifecycle.ReadSpec(filepath.Join(root, "agent", "spec.json"))
	switch {
	case err != nil:
		fail("spec.json: %v", err)
		failed = true
	default:
		ok("spec.json parses (%d env refs, %d if_env guards)", len(info.EnvRefs), len(info.IfEnvNames))
	}

	tag, err := lifecycle.BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
	switch {
	case err != nil:
		fail("Dockerfile base tag: %v", err)
		failed = true
	default:
		ok("base image pinned: ghcr.io/tankdonut/agent-base:%s", tag)
	}

	if n, err := lifecycle.SecretsCheck(root); err != nil {
		fail("secrets: %v", err)
		failed = true
	} else {
		ok("agent/.env sets all %d required vars", n)
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

	if failed {
		return fmt.Errorf("doctor found problems — fix the FAIL lines above")
	}
	fmt.Fprintln(out, "all checks passed")
	return nil
}
