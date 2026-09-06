package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/platform/fly"
)

// newPlatformCmd manages the project's deployment platform: list the
// registry, pin one in .agentctl.yaml, and lint the repo-owned
// manifests against the image contract.
func newPlatformCmd() *cobra.Command {
	platformCmd := &cobra.Command{
		Use:   "platform",
		Short: "Manage the deployment platform",
		Long: `The platform is a project-level choice: it names where the release
verbs (deploy, status, logs, mcp, stop, start, destroy) operate.
compose is the default; pin another with ` + "`agentctl platform set`" + `.`,
	}

	var ls = &cobra.Command{
		Use:   "ls",
		Short: "List available platforms and mark the pinned one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, info := range platformInfos() {
				marker := "        "
				if info.name == cfg.Platform {
					marker = "pinned  "
				} else if info.defaultPlatform {
					marker = "default "
				}
				fmt.Fprintf(out, "%s%s — %s\n", marker, info.name, info.description)
			}
			return nil
		},
	}

	var set = &cobra.Command{
		Use:   "set <name>",
		Short: "Pin the platform in .agentctl.yaml and lint the project",
		Long: `Pins the platform and scaffolds its repo-owned manifest when the
adapter has one: fly writes deploy/fly.toml (--app and --region are
required for it, and an existing manifest is never overwritten).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			switch args[0] {
			case "fly":
				if err := fly.ScaffoldConfig(root, setApp, setRegion); err != nil {
					return err
				}
			}
			p, err := forPlatform(args[0], newRunner(), cfg.Namespaces)
			if err != nil {
				return err
			}
			d, err := platform.Derive(root)
			if err != nil {
				return err
			}
			if err := p.Check(root, &d); err != nil {
				return fmt.Errorf("not pinning: %w", err)
			}
			if err := pinPlatform(root, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pinned platform %q in %s\n", args[0], ConfigName)
			if args[0] == "fly" {
				fmt.Fprintf(cmd.OutOrStdout(), "next: `fly launch --no-deploy --copy-config -c deploy/fly.toml` once, then `fly secrets import < agent/.env` — then `agentctl deploy`\n")
			}
			return nil
		},
	}
	set.Flags().StringVar(&setApp, "app", "", "fly app name (fly scaffold; required for fly)")
	set.Flags().StringVar(&setRegion, "region", "", "primary fly region code (fly scaffold; default iad — US East)")

	var check = &cobra.Command{
		Use:   "check",
		Short: "Lint repo-owned manifests against the image contract",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			if err := p.Check(root, &d); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "platform %q check passed for %s (base tag %s)\n", p.Name(), d.Project, d.BaseTag)
			return nil
		},
	}

	platformCmd.AddCommand(ls, set, check)
	return platformCmd
}

var (
	setApp    string
	setRegion string
)
