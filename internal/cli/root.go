// Package cli wires agentctl's cobra command tree over the pure
// internal/lifecycle engine and the internal/scaffold renderer.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Version is the single agentctl version constant, date-versioned in the
// same YYYY.MM.DD[.N] scheme as the agent-base image tags.
const Version = "2026.08.29"

// NewRootCommand builds the full agentctl command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "agentctl",
		Short: "Operator CLI for downstream agent-base projects",
		Long: `agentctl — operator CLI for downstream agent projects on the
agent-base image: scaffolding, compose lifecycle, secrets, worktrees,
and validation.

Configuration (optional): engine (auto|podman|docker) and gateway_port
via .agentctl.yaml or AGENTCTL_ENGINE / AGENTCTL_GATEWAY_PORT.

Exit codes: 0 success, 1 any error — usage and flag errors included.`,
		Version:      Version,
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return setupViper()
		},
	}
	root.SetVersionTemplate("agentctl {{.Version}}\n")
	root.AddCommand(
		newInitCmd(),
		newVersionCmd(),
		newSecretsCmd(),
		newEnvCmd(),
		newWorktreeCmd(),
	)
	root.AddCommand(newLifecycleCmds()...)
	root.AddCommand(newMiscCmds()...)
	return root
}

// Execute runs the root command and maps the result to a process exit
// code: 0 success, 1 any error (usage and flag errors included).
func Execute() int {
	if err := NewRootCommand().Execute(); err != nil {
		return 1
	}
	return 0
}

// setupViper layers configuration: defaults, then an optional project
// .agentctl.yaml in the working directory, then AGENTCTL_* env vars.
func setupViper() error {
	viper.SetDefault("engine", "auto")
	viper.SetDefault("gateway_port", 18789)
	viper.SetEnvPrefix("AGENTCTL")
	viper.AutomaticEnv()
	if _, err := os.Stat(".agentctl.yaml"); err == nil {
		viper.SetConfigFile(".agentctl.yaml")
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("reading .agentctl.yaml: %w", err)
		}
	}
	return nil
}
