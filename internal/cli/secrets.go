package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

func newSecretsCmd() *cobra.Command {
	secrets := &cobra.Command{
		Use:   "secrets",
		Short: "Manage agent/.env",
	}
	var init = &cobra.Command{
		Use:   "init",
		Short: "Create agent/.env from the example with a generated gateway token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretsInit(cmd)
		},
	}
	var check = &cobra.Command{
		Use:   "check",
		Short: "Check every required env var is set in agent/.env",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			n, err := lifecycle.SecretsCheck(root)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "agent/.env OK: %d required vars set\n", n)
			return nil
		},
	}
	var edit = &cobra.Command{
		Use:   "edit",
		Short: "Edit agent/.env in $EDITOR (default vi)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			editor := os.Getenv("EDITOR")
			return lifecycle.SecretsEdit(newRunner(), editor, filepath.Join(root, "agent", ".env"))
		},
	}
	secrets.AddCommand(init, check, edit)
	return secrets
}

// newEnvCmd is the root-level alias for `agentctl secrets init`.
func newEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "Alias for `agentctl secrets init`",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runSecretsInit(cmd) },
	}
}

func runSecretsInit(cmd *cobra.Command) error {
	root, err := chdirProject()
	if err != nil {
		return err
	}
	path, err := lifecycle.SecretsInit(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (mode 0600) with a generated OPENCLAW_GATEWAY_TOKEN\n", path)
	fmt.Fprintln(cmd.OutOrStdout(), "fill in the remaining vars with `agentctl secrets edit`")
	return nil
}
