package cli

import (
	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

func newMiscCmds() []*cobra.Command {
	var check = &cobra.Command{
		Use:   "check",
		Short: "Run pre-commit on all files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			return lifecycle.PreCommitCheck(newRunner())
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
			return lifecycle.PreCommitHooks(newRunner())
		},
	}
	return []*cobra.Command{check, hooks}
}
