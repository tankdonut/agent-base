package cli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

// chdirProject locates the enclosing agent project, chdirs to its root
// (compose and validate use root-relative paths), and returns the
// absolute root.
func chdirProject() (string, error) {
	root, err := lifecycle.FindProjectRoot(".")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := os.Chdir(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// resolveEngine maps the configured engine preference to a binary.
func resolveEngine() (string, error) {
	return lifecycle.ResolveEngine(viper.GetString("engine"), newRunner())
}

func newLifecycleCmds() []*cobra.Command {
	var up = &cobra.Command{
		Use:   "up",
		Short: "Start the agent stack (compose up -d)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, engine, err := projectEngine()
			if err != nil {
				return err
			}
			lifecycle.WarnGatewayPortBusy(cmd.ErrOrStderr(), root, viper.GetInt("gateway_port"))
			return lifecycle.Up(newRunner(), engine, root)
		},
	}
	var dev = &cobra.Command{
		Use:   "dev",
		Short: "Start the stack with the dev overlay (hot-reload mounts)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, engine, err := projectEngine()
			if err != nil {
				return err
			}
			lifecycle.WarnGatewayPortBusy(cmd.ErrOrStderr(), root, viper.GetInt("gateway_port"))
			return lifecycle.Dev(newRunner(), engine, root)
		},
	}
	var down = &cobra.Command{
		Use:   "down",
		Short: "Stop the agent stack",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			engine, err := resolveEngine()
			if err != nil {
				return err
			}
			return lifecycle.Down(newRunner(), engine)
		},
	}
	var logs = &cobra.Command{
		Use:                "logs [args...]",
		Short:              "Show compose logs (args pass through, e.g. -f agent)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			engine, err := resolveEngine()
			if err != nil {
				return err
			}
			return lifecycle.Logs(newRunner(), engine, args)
		},
	}
	var mcp = &cobra.Command{
		Use:                "mcp [args...]",
		Short:              "Run openclaw mcp inside the agent container (login, logout, status, doctor)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			engine, err := resolveEngine()
			if err != nil {
				return err
			}
			return lifecycle.Mcp(newRunner(), engine, args)
		},
	}
	var buildImages = &cobra.Command{
		Use:   "build-images",
		Short: "Build the project image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			engine, err := resolveEngine()
			if err != nil {
				return err
			}
			return lifecycle.BuildImages(newRunner(), engine)
		},
	}
	var restart = &cobra.Command{
		Use:   "restart [svc...]",
		Short: "Restart services (all when none given)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			engine, err := resolveEngine()
			if err != nil {
				return err
			}
			return lifecycle.Restart(newRunner(), engine, args)
		},
	}
	var rebuild = &cobra.Command{
		Use:   "rebuild [svc...]",
		Short: "Rebuild images and force-recreate services",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := chdirProject(); err != nil {
				return err
			}
			engine, err := resolveEngine()
			if err != nil {
				return err
			}
			return lifecycle.Rebuild(newRunner(), engine, args)
		},
	}
	var update = &cobra.Command{
		Use:   "update",
		Short: "git pull --ff-only, then rebuild the stack",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, engine, err := projectEngine()
			if err != nil {
				return err
			}
			return lifecycle.Update(newRunner(), engine, root)
		},
	}
	var validate = &cobra.Command{
		Use:   "validate",
		Short: "Validate spec + automations via the base image (--validate-spec)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, engine, err := projectEngine()
			if err != nil {
				return err
			}
			return lifecycle.Validate(newRunner(), engine, root)
		},
	}
	return []*cobra.Command{up, dev, down, logs, mcp, buildImages, restart, rebuild, update, validate}
}

// projectEngine resolves the project root (chdir'ing into it) and the
// engine in one step.
func projectEngine() (string, string, error) {
	root, err := chdirProject()
	if err != nil {
		return "", "", err
	}
	engine, err := resolveEngine()
	if err != nil {
		return "", "", err
	}
	return root, engine, nil
}
