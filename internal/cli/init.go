package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/scaffold"
)

func newInitCmd() *cobra.Command {
	var cfg scaffold.Config
	cmd := &cobra.Command{
		Use:   "init <dir>",
		Short: "Scaffold a new agent project into <dir>",
		Long:  "Scaffold a new downstream agent repository (spec, compose, workspace, automations) consuming the agent-base image.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolving target dir: %w", err)
			}
			cfg.TargetDir = abs
			cfg.ProjectName = filepath.Base(abs)
			if cfg.AgentName == "" {
				cfg.AgentName = scaffold.DefaultAgentName(cfg.ProjectName)
			}
			created, err := scaffold.Run(cfg)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, rel := range created {
				fmt.Fprintln(out, rel)
			}
			fmt.Fprintf(out, "\nNext steps:\n  cd %s\n  agentctl secrets init\n  agentctl secrets edit\n  agentctl up\n", cfg.TargetDir)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.AgentName, "agent-name", "", "agent name (default: title-cased dir basename)")
	f.StringVar(&cfg.BaseTag, "base-tag", scaffold.DefaultBaseTag, "agent-base image tag, YYYY.MM.DD[.N]")
	f.StringVar(&cfg.Model, "model", scaffold.DefaultModel, "fallback and automations model")
	f.IntVar(&cfg.GatewayPort, "gateway-port", scaffold.DefaultGatewayPort, "host gateway port")
	f.BoolVar(&cfg.Telegram, "telegram", true, "wire the telegram channel")
	f.BoolVar(&cfg.GitInit, "git-init", false, "run git init in the target after scaffolding")
	f.BoolVar(&cfg.Force, "force", false, "overwrite an existing non-empty target directory")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the agentctl version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "agentctl %s\n", Version)
			return nil
		},
	}
}
