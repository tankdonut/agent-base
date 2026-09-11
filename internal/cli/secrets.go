package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// SecretsEdit opens agent/.env in the user's editor — process glue like
// Open and PreCommitCheck; the on-disk secrets contract lives in
// internal/project.
func SecretsEdit(r process.Runner, editor, envPath string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if editor == "" {
		editor = "vi"
	}
	if _, err := process.LookPath(r, editor); err != nil {
		return fmt.Errorf("editor %q not found in PATH (set $EDITOR)", editor)
	}
	return process.RunArgv(r, nil, editor, envPath)
}

func newSecretsCmd() *cobra.Command {
	secrets := &cobra.Command{
		Use:   "secrets",
		Short: "Manage agent/.env (and litellm/.env when the project ships one)",
	}
	var init = &cobra.Command{
		Use:   "init",
		Short: "Create agent/.env (plus litellm/.env when present) with generated secrets",
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
			n, err := project.SecretsCheck(root)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "agent/.env OK: %d required vars set\n", n)
			return nil
		},
	}
	var edit = &cobra.Command{
		Use:   "edit",
		Short: "Edit agent/.env (then litellm/.env) in $EDITOR (default vi)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			editor := os.Getenv("EDITOR")
			if err := SecretsEdit(newRunner(), editor, filepath.Join(root, "agent", ".env")); err != nil {
				return err
			}
			litellmPath := filepath.Join(root, "litellm", ".env")
			if _, err := os.Stat(litellmPath); err == nil {
				return SecretsEdit(newRunner(), editor, litellmPath)
			}
			return nil
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
	paths, err := project.SecretsInit(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (mode 0600) with a generated OPENCLAW_GATEWAY_TOKEN\n", paths[0])
	for _, p := range paths[1:] {
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (mode 0600) with a generated LITELLM_MASTER_KEY mirrored into agent/.env as LITELLM_API_KEY\n", p)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "fill in the remaining vars with `agentctl secrets edit`")
	return nil
}
