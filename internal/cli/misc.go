package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/process"
)

// PreCommitCheck runs `pre-commit run --all-files` in the project.
func PreCommitCheck(r process.Runner) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if _, err := process.LookPath(r, "pre-commit"); err != nil {
		return fmt.Errorf("pre-commit not found in PATH — install it (e.g. `pipx install pre-commit`) and re-run")
	}
	return process.RunArgv(r, nil, "pre-commit", "run", "--all-files")
}

// PreCommitHooks installs pre-commit's git hooks in the project.
func PreCommitHooks(r process.Runner) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if _, err := process.LookPath(r, "pre-commit"); err != nil {
		return fmt.Errorf("pre-commit not found in PATH — install it (e.g. `pipx install pre-commit`) and re-run")
	}
	return process.RunArgv(r, nil, "pre-commit", "install")
}

func newMiscCmds() []*cobra.Command {
	var check = &cobra.Command{
		Use:   "check",
		Short: "Run pre-commit on all files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			return PreCommitCheck(newRunner())
		},
	}
	var hooks = &cobra.Command{
		Use:   "hooks",
		Short: "Install pre-commit git hooks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			return PreCommitHooks(newRunner())
		},
	}
	return []*cobra.Command{check, hooks}
}
