// gatewaystatus.go hosts the WS-backed operator surface: the
// --live flavor of `fleet status` (per-agent gateway probe over the
// pinned protocol) and the `approvals` verb (list pending, resolve by
// ID). Both read the per-agent gateway token from agents/<name>/.env
// and reach the gateway on its loopback-published port — neither
// prints token material.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/gatewayclient"
	"github.com/tankdonut/agent-base/internal/process"
)

// gatewayToken reads OPENCLAW_GATEWAY_TOKEN from the agent's .env.
func gatewayToken(agentDir string) string {
	data, err := os.ReadFile(filepath.Join(agentDir, ".env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && key == "OPENCLAW_GATEWAY_TOKEN" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// gatewayRow is one agent's live gateway summary.
type gatewayRow struct {
	Agent     string
	OK        bool
	Detail    string
	Sessions  int
	Approvals int
}

// probeGateway connects to one agent's loopback gateway and collects
// the live summary. Every failure is a row, never a fatal error —
// live status must degrade per agent like the compose path.
func probeGateway(ctx context.Context, name, agentDir, engine, deviceKey string, port int) gatewayRow {
	row := gatewayRow{Agent: name}
	token := gatewayToken(agentDir)
	if token == "" {
		row.Detail = "no OPENCLAW_GATEWAY_TOKEN in .env"
		return row
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	c, err := gatewayclient.Connect(probeCtx, fmt.Sprintf("ws://127.0.0.1:%d", port), token, gatewayclient.Options{
		DeviceKeyPath: deviceKey,
		ApproveDevice: func() error { return compose.ApproveOwnDevice(newRunner(), engine, agentDir) },
	})
	if err != nil {
		row.Detail = err.Error()
		return row
	}
	defer c.Close()
	row.OK = true
	row.Detail = c.ServerVersion()
	var sessions struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := c.Call(probeCtx, "sessions.list", map[string]any{}, &sessions); err == nil {
		row.Sessions = len(sessions.Sessions)
	}
	approvals, err := c.ListApprovals(probeCtx)
	if err == nil {
		row.Approvals = len(approvals)
	}
	return row
}

// printGatewayRows renders the live table.
func printGatewayRows(out interface{ Write([]byte) (int, error) }, rows []gatewayRow) {
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tGATEWAY\tSESSIONS\tAPPROVALS")
	for _, r := range rows {
		state := "FAIL: " + r.Detail
		if r.OK {
			state = "ok (" + r.Detail + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", r.Agent, state, r.Sessions, r.Approvals)
	}
	_ = tw.Flush()
}

// runFleetLiveStatus probes every scoped agent's gateway directly.
func runFleetLiveStatus(cmd *cobra.Command, agentFlag string, all bool) error {
	m, names, err := fleetScope(agentFlag, all)
	if err != nil {
		return err
	}
	engine, engineErr := process.ResolveEngine(m.Defaults.ComposeEngine, newRunner())
	if engineErr != nil {
		return engineErr
	}
	deviceKey := deviceKeyPath(m.Root)
	rows := make([]gatewayRow, 0, len(names))
	for _, name := range names {
		entry := m.Agents[name]
		rows = append(rows, probeGateway(cmd.Context(), name, entry.Dir, engine, deviceKey, entry.GatewayPort))
	}
	printGatewayRows(cmd.OutOrStdout(), rows)
	for _, r := range rows {
		if !r.OK {
			return fmt.Errorf("agent %s: %s", r.Agent, r.Detail)
		}
	}
	return nil
}

// newApprovalsCmd is the operator surface for pending approvals:
// default lists them (exec + plugin families); --id with --approve or
// --deny resolves one. Approval targets ONE agent — the default
// fleet-of-one scope, otherwise an explicit --agent.
func newApprovalsCmd() *cobra.Command {
	var agentFlag, id string
	var approveBool, denyBool bool

	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List or resolve the agent's pending approvals (exec/plugin)",
		Long: `Lists pending exec and plugin approvals through the agent's
gateway. Resolve one with --id plus --approve or --deny — approval is
never batched: pass --agent for anything beyond a fleet of one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			decision := ""
			switch {
			case approveBool && denyBool:
				return fmt.Errorf("--approve and --deny are mutually exclusive")
			case approveBool:
				decision = gatewayclient.DecisionApprove
			case denyBool:
				decision = gatewayclient.DecisionDeny
			}
			if id != "" && decision == "" {
				return fmt.Errorf("--id requires --approve or --deny")
			}
			if id == "" && decision != "" {
				return fmt.Errorf("--approve/--deny need --id")
			}
			m, err := loadFleet()
			if err != nil {
				return err
			}
			var name string
			switch {
			case agentFlag != "":
				if _, ok := m.Agents[agentFlag]; !ok {
					return fmt.Errorf("agent %q is not registered in %s", agentFlag, fleet.ManifestName)
				}
				name = agentFlag
			case len(m.Agents) == 1:
				name = m.AgentNames()[0]
			default:
				return fmt.Errorf("%d agents registered — approvals demand an explicit --agent", len(m.Agents))
			}
			entry := m.Agents[name]
			token := gatewayToken(entry.Dir)
			if token == "" {
				return fmt.Errorf("agents/%s/.env carries no OPENCLAW_GATEWAY_TOKEN", name)
			}
			engine, engineErr := process.ResolveEngine(m.Defaults.ComposeEngine, newRunner())
			if engineErr != nil {
				return engineErr
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()
			c, err := gatewayclient.Connect(ctx, fmt.Sprintf("ws://127.0.0.1:%d", entry.GatewayPort), token, gatewayclient.Options{
				DeviceKeyPath: deviceKeyPath(m.Root),
				ApproveDevice: func() error { return compose.ApproveOwnDevice(newRunner(), engine, entry.Dir) },
			})
			if err != nil {
				return fmt.Errorf("gateway unreachable for %s (is the agent up?): %w", name, err)
			}
			defer c.Close()

			if id != "" {
				family := "exec"
				if strings.HasPrefix(id, "pl_") {
					family = "plugin"
				}
				if err := c.ResolveApproval(ctx, family, id, decision); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s)\n", decision, id, name)
				return nil
			}
			approvals, err := c.ListApprovals(ctx)
			if err != nil {
				return err
			}
			if len(approvals) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: no pending approvals\n", name)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tCOMMAND/SUMMARY\tAGENT")
			for _, a := range approvals {
				what := a.Summary
				if a.Command != "" {
					what = a.Command
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.ID, a.Kind, what, a.Agent)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&agentFlag, "agent", "", "operate one named agent")
	cmd.Flags().StringVar(&id, "id", "", "resolve this approval ID instead of listing")
	cmd.Flags().BoolVar(&approveBool, "approve", false, "approve --id")
	cmd.Flags().BoolVar(&denyBool, "deny", false, "deny --id")
	return cmd
}
