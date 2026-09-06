package cli

import (
	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/project"
)

// newDevCmd is the local iterative surface: the compose stack with the
// hot-reload overlay always applied. Dev is a target, not a mode of
// every verb — release verbs live at the top level and dispatch through
// the platform port.
func newDevCmd() *cobra.Command {
	dev := &cobra.Command{
		Use:   "dev",
		Short: "Local dev loop (compose + hot-reload overlay)",
		Long: `Local iterative lifecycle: the project's compose stack with
compose.dev.yml applied — workspace/skills/knowledge bind-mounted,
seeding skipped, edits live without a rebuild.

Subcommands: up, down, logs, restart, mcp, open.`,
	}
	var up = &cobra.Command{
		Use:   "up",
		Short: "Start the dev stack (overlay applied)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, engine, err := devEngine()
			if err != nil {
				return err
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			project.WarnGatewayPortBusy(cmd.ErrOrStderr(), root, cfg.ComposeGatewayPort())
			return compose.Dev(newRunner(), engine, root)
		},
	}
	var down = &cobra.Command{
		Use:   "down",
		Short: "Stop the dev stack (volumes kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, engine, err := devEngine()
			if err != nil {
				return err
			}
			return compose.Down(newRunner(), engine)
		},
	}
	var logs = &cobra.Command{
		Use:                "logs [args...]",
		Short:              "Follow dev stack logs (args pass through, e.g. -f agent)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, engine, err := devEngine()
			if err != nil {
				return err
			}
			return compose.Logs(newRunner(), engine, args)
		},
	}
	var restart = &cobra.Command{
		Use:   "restart [svc...]",
		Short: "Restart services (all when none given)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, engine, err := devEngine()
			if err != nil {
				return err
			}
			return compose.Restart(newRunner(), engine, args)
		},
	}
	var mcp = &cobra.Command{
		Use:                "mcp [args...]",
		Short:              "Run openclaw mcp in the dev container (login, logout, status, doctor)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, engine, err := devEngine()
			if err != nil {
				return err
			}
			return compose.Mcp(newRunner(), engine, args)
		},
	}
	var open = &cobra.Command{
		Use:   "open",
		Short: "Print and open the gateway URL (xdg-open)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			return Open(newRunner(), root, cfg.ComposeGatewayPort(), cmd.OutOrStdout())
		},
	}
	dev.AddCommand(up, down, logs, restart, mcp, open)
	return dev
}

// devEngine resolves the project root and the local compose engine in
// one step for the dev group.
func devEngine() (string, string, error) {
	root, err := chdirProject()
	if err != nil {
		return "", "", err
	}
	engine, err := resolveComposeEngine()
	if err != nil {
		return "", "", err
	}
	return root, engine, nil
}
