package cli

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

func newMiscCmds() []*cobra.Command {
	var open = &cobra.Command{
		Use:   "open",
		Short: "Print and open the gateway URL (xdg-open)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			return lifecycle.Open(newRunner(), root, viper.GetInt("gateway_port"), cmd.OutOrStdout())
		},
	}
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
	return []*cobra.Command{open, check, hooks}
}
