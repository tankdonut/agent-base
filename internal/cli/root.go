// Package cli wires agentctl's cobra command tree over the pure
// internal/lifecycle engine and the internal/scaffold renderer.
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

// Version is the single agentctl version constant, date-versioned in the
// same YYYY.MM.DD[.N] scheme as the agent-base image tags.
const Version = "2026.09.05"

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

// setupViper layers configuration: defaults, then an optional
// .agentctl.yaml (working directory, else the project root), then
// AGENTCTL_* env vars.
func setupViper() error {
	viper.SetDefault("engine", "auto")
	viper.SetDefault("gateway_port", 18789)
	viper.SetEnvPrefix("AGENTCTL")
	viper.AutomaticEnv()
	path := agentctlConfigPath()
	if path == "" {
		return nil
	}
	viper.SetConfigFile(path)
	if err := viper.ReadInConfig(); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}

// agentctlConfigPath resolves the config file: a .agentctl.yaml in the
// working directory wins; otherwise the project root's copy — commands
// resolve the project by marker from any subdirectory, so the config
// must follow the same root, not the invocation cwd.
func agentctlConfigPath() string {
	if _, err := os.Stat(".agentctl.yaml"); err == nil {
		return ".agentctl.yaml"
	}
	root, err := lifecycle.FindProjectRoot(".")
	if err != nil {
		return "" // not inside a project: defaults + env only
	}
	candidate := filepath.Join(root, ".agentctl.yaml")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}
