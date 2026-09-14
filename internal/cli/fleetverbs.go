// fleetverbs.go hosts the fleet release verbs: the same Platform-port
// vocabulary as the single-agent verbs, fanned out over roster agents
// with per-agent failure isolation. --all is never the default: a
// fleet of one resolves implicitly, anything larger demands an explicit
// scope (--agent or --all) so batch effects are always deliberate.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/platform"
)

// fleetScope resolves which roster entries a fleet verb operates on:
// --agent X (validated against the registry), --all, or the implicit
// fleet-of-one. Bigger fleets without an explicit scope are an error —
// never a silent batch.
func fleetScope(agentFlag string, all bool) (*fleet.Manifest, []string, error) {
	m, err := loadFleet()
	if err != nil {
		return nil, nil, err
	}
	if agentFlag != "" && all {
		return nil, nil, fmt.Errorf("--agent and --all are mutually exclusive")
	}
	switch {
	case agentFlag != "":
		if _, ok := m.Agents[agentFlag]; !ok {
			return nil, nil, fmt.Errorf("agent %q is not registered in %s (known: %v)", agentFlag, fleet.ManifestName, m.AgentNames())
		}
		return m, []string{agentFlag}, nil
	case all:
		return m, m.AgentNames(), nil
	case len(m.Agents) == 1:
		return m, m.AgentNames(), nil
	default:
		return nil, nil, fmt.Errorf("%d agents registered — pass --agent <name> or --all (batch verbs are never implicit)", len(m.Agents))
	}
}

// runFleetVerb fans one release verb over the scoped agents. Each agent
// resolves independently (envelope render, platform, deployment); one
// failure never stops the others. The per-agent summary is the last
// word, and any failure exits 1.
func runFleetVerb(cmd *cobra.Command, agentFlag string, all bool, verb func(root string, p platform.Platform, d *platform.Deployment) error) error {
	m, names, err := fleetScope(agentFlag, all)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	failed := 0
	for _, name := range names {
		if len(names) > 1 {
			fmt.Fprintf(out, "==> %s\n", name)
		}
		root, p, d, err := loadProjectPlatformFor(m, name)
		if err == nil {
			err = verb(root, p, &d)
		}
		if err != nil {
			failed++
			fmt.Fprintf(errOut, "FAIL %s: %v\n", name, err)
		}
	}
	fmt.Fprintf(out, "%d agent(s): %d ok, %d failed\n", len(names), len(names)-failed, failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d agent(s) failed — see the FAIL lines above", failed, len(names))
	}
	return nil
}

// newFleetVerbCmds builds fleet deploy/status/logs/backup/stop/start,
// each carrying --agent/--all. logs follows a single stream, so it
// demands a single target (the fleet-of-one default included).
func newFleetVerbCmds() []*cobra.Command {
	var agentFlag string
	var all bool
	var dryRun, force, follow, live bool
	withScope := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().StringVar(&agentFlag, "agent", "", "operate one named agent")
		cmd.Flags().BoolVar(&all, "all", false, "operate every registered agent")
		return cmd
	}

	var deploy = &cobra.Command{
		Use:   "deploy",
		Short: "Converge the scoped agent(s) onto their platforms",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetVerb(cmd, agentFlag, all, func(root string, p platform.Platform, d *platform.Deployment) error {
				return p.Deploy(cmd.Context(), newRunner(), root, d, platform.DeployOptions{DryRun: dryRun, Force: force}, cmdOut{cmd.OutOrStdout()})
			})
		},
	}
	deploy.Flags().BoolVar(&dryRun, "dry-run", false, "run the contract check only, no side effects")
	deploy.Flags().BoolVar(&force, "force", false, "recreate containers even when the image did not change")

	var status = &cobra.Command{
		Use:   "status",
		Short: "Where the scoped agent(s) stand",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if live {
				return runFleetLiveStatus(cmd, agentFlag, all)
			}
			return runFleetVerb(cmd, agentFlag, all, func(root string, p platform.Platform, d *platform.Deployment) error {
				return p.Status(cmd.Context(), newRunner(), root, d, cmdOut{cmd.OutOrStdout()})
			})
		},
	}
	status.Flags().BoolVar(&live, "live", false, "probe each agent's gateway over WS (version, sessions, pending approvals)")

	var logs = &cobra.Command{
		Use:   "logs",
		Short: "Follow one agent's logs (-f to keep the stream open)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetVerb(cmd, agentFlag, all, func(root string, p platform.Platform, d *platform.Deployment) error {
				return p.Logs(cmd.Context(), newRunner(), root, d, follow, cmdOut{cmd.OutOrStdout()})
			})
		},
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep the log stream open")

	var backup = &cobra.Command{
		Use:   "backup",
		Short: "Verified backups of the scoped agent(s)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetVerb(cmd, agentFlag, all, func(root string, p platform.Platform, d *platform.Deployment) error {
				if !p.Capabilities().Exec {
					return fmt.Errorf("platform %q cannot exec into a running instance", p.Name())
				}
				return p.Backup(cmd.Context(), newRunner(), root, d, cmdOut{cmd.OutOrStdout()})
			})
		},
	}

	var stop = &cobra.Command{
		Use:   "stop",
		Short: "Pause the scoped agent(s) without destroying them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetVerb(cmd, agentFlag, all, func(root string, p platform.Platform, d *platform.Deployment) error {
				if !p.Capabilities().StopStart {
					return fmt.Errorf("platform %q has no stop/start", p.Name())
				}
				return p.Stop(cmd.Context(), newRunner(), root, d, cmdOut{cmd.OutOrStdout()})
			})
		},
	}

	var start = &cobra.Command{
		Use:   "start",
		Short: "Resume the scoped agent(s)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetVerb(cmd, agentFlag, all, func(root string, p platform.Platform, d *platform.Deployment) error {
				if !p.Capabilities().StopStart {
					return fmt.Errorf("platform %q has no stop/start", p.Name())
				}
				return p.Start(cmd.Context(), newRunner(), root, d, cmdOut{cmd.OutOrStdout()})
			})
		},
	}

	return []*cobra.Command{
		withScope(deploy),
		withScope(status),
		withScope(logs),
		withScope(backup),
		withScope(stop),
		withScope(start),
	}
}
