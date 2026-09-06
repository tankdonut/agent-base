package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/platform"
)

// newReleaseCmds builds the release verbs: one vocabulary dispatched
// through the platform port. The platform comes from config (default
// compose), so `deploy` on this host and `deploy` on a remote platform
// are the same operation through the same interface.
func newReleaseCmds() []*cobra.Command {
	var dryRun, force, follow, destroyVolumes, destroyYes bool

	var deploy = &cobra.Command{
		Use:   "deploy",
		Short: "Converge the agent onto its platform (build, up, health)",
		Long: `Ship the checked-out tree to the configured platform: contract
check, build, converge, and report. Idempotent — the platform is the
state store. Pull first yourself: deploy ships what is checked out,
deliberately (git pull --ff-only && agentctl deploy).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			opts := platform.DeployOptions{DryRun: dryRun, Force: force}
			return p.Deploy(cmd.Context(), newRunner(), root, &d, opts, cmdOut{cmd.OutOrStdout()})
		},
	}
	deploy.Flags().BoolVar(&dryRun, "dry-run", false, "run the contract check only, no side effects")
	deploy.Flags().BoolVar(&force, "force", false, "recreate containers even when the image did not change")

	var status = &cobra.Command{
		Use:   "status",
		Short: "Where the instance stands (platform state, tag, health)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			return p.Status(cmd.Context(), newRunner(), root, &d, cmdOut{cmd.OutOrStdout()})
		},
	}

	var logs = &cobra.Command{
		Use:   "logs",
		Short: "Instance logs (-f to follow)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			return p.Logs(cmd.Context(), newRunner(), root, &d, follow, cmdOut{cmd.OutOrStdout()})
		},
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep the log stream open")

	var mcp = &cobra.Command{
		Use:                "mcp [args...]",
		Short:              "Run openclaw mcp against the running instance (capability-gated)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			if !p.Capabilities().Exec {
				return fmt.Errorf("platform %q cannot exec into a running instance — run `agentctl dev mcp` against a local dev stack instead", p.Name())
			}
			return p.Mcp(cmd.Context(), newRunner(), root, &d, args, cmdOut{cmd.OutOrStdout()})
		},
	}

	var stop = &cobra.Command{
		Use:   "stop",
		Short: "Pause the instance without destroying it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			if !p.Capabilities().StopStart {
				return fmt.Errorf("platform %q has no stop/start — use `agentctl destroy` (volumes are kept by default)", p.Name())
			}
			return p.Stop(cmd.Context(), newRunner(), root, &d, cmdOut{cmd.OutOrStdout()})
		},
	}

	var start = &cobra.Command{
		Use:   "start",
		Short: "Resume a stopped instance",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			if !p.Capabilities().StopStart {
				return fmt.Errorf("platform %q has no stop/start — use `agentctl deploy` to bring the instance back", p.Name())
			}
			return p.Start(cmd.Context(), newRunner(), root, &d, cmdOut{cmd.OutOrStdout()})
		},
	}

	var destroy = &cobra.Command{
		Use:   "destroy",
		Short: "Tear the instance down (volumes kept unless --volumes)",
		Long: `Remove the stack from the platform. The warm volumes survive by
default — data safety beats availability. --volumes deletes agent-data
and agent-backups too and asks for the project name as confirmation
(skip with --yes).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, p, d, err := loadProjectPlatform()
			if err != nil {
				return err
			}
			if !p.Capabilities().VolumePreservingDestroy && !destroyVolumes {
				return fmt.Errorf("platform %q always deletes the data volume when destroying — pass --volumes to accept that, or use `agentctl stop` to pause while keeping data", p.Name())
			}
			if destroyVolumes && !destroyYes {
				if err := confirmDestroy(cmd.InOrStdin(), cmd.OutOrStdout(), d.Project); err != nil {
					return err
				}
			}
			return p.Destroy(cmd.Context(), newRunner(), root, &d, destroyVolumes, cmdOut{cmd.OutOrStdout()})
		},
	}
	destroy.Flags().BoolVar(&destroyVolumes, "volumes", false, "also delete the persistent volumes (agent-data, agent-backups)")
	destroy.Flags().BoolVar(&destroyYes, "yes", false, "skip the --volumes confirmation prompt")

	return []*cobra.Command{deploy, status, logs, mcp, stop, start, destroy}
}

// cmdOut adapts cobra's stdout to the platform Output port.
type cmdOut struct{ w io.Writer }

func (o cmdOut) Printf(format string, a ...any) {
	fmt.Fprintf(o.w, format, a...)
}

// confirmDestroy asks the operator to type the project name before
// irreversible data loss; anything else aborts.
func confirmDestroy(in io.Reader, out io.Writer, project string) error {
	fmt.Fprintf(out, "this deletes the persistent volumes of %q — type the project name to confirm: ", project)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(line) != project {
		return fmt.Errorf("aborted — volumes kept")
	}
	return nil
}
