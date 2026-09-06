// Package cli wires agentctl's cobra command tree over the pure
// internal/lifecycle engine, the internal/platform deployment port,
// and the internal/scaffold renderer.
package cli

import (
	"github.com/spf13/cobra"
)

// Version is the single agentctl version constant, date-versioned in the
// same YYYY.MM.DD[.N] scheme as the agent-base image tags.
const Version = "2026.09.05"

// NewRootCommand builds the full agentctl command tree: repo tooling
// (init, platform, secrets, worktree, validate, doctor), the local dev
// group, and the release verbs dispatched through the platform port.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "agentctl",
		Short: "Operator CLI for downstream agent-base projects",
		Long: `agentctl — operator CLI for downstream agent projects on the
agent-base image: scaffolding, the local dev loop, deployment
platforms, secrets, worktrees, and validation.

Platforms select where the release verbs (deploy, status, logs, mcp,
stop, start, destroy) operate; compose is the default and the reference
adapter. Configure via .agentctl.yaml (platform, compose.engine,
compose.gateway_port) or AGENTCTL_PLATFORM / AGENTCTL_COMPOSE_ENGINE /
AGENTCTL_COMPOSE_GATEWAY_PORT.

Exit codes: 0 success, 1 any error — usage and flag errors included.`,
		Version:      Version,
		SilenceUsage: true,
	}
	root.SetVersionTemplate("agentctl {{.Version}}\n")
	root.AddCommand(
		newInitCmd(),
		newVersionCmd(),
		newSecretsCmd(),
		newEnvCmd(),
		newWorktreeCmd(),
		newValidateCmd(),
		newDoctorCmd(),
		newPlatformCmd(),
		newDevCmd(),
	)
	root.AddCommand(newReleaseCmds()...)
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
