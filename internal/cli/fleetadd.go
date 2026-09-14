// fleetadd.go hosts `fleet add`: register a new agent in an existing
// fleet repo — scaffold its scoped content, allocate the next free
// gateway port, and edit fleet.yaml in place (comments survive).
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/scaffold"
)

func newFleetAddCmd() *cobra.Command {
	var port int
	var agentName, baseTag string
	var telegram bool

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Scaffold + register a new agent in the fleet",
		Long: `Emits the agent-scoped scaffold tree under agents/<name>/ and
registers the entry in fleet.yaml with the next free gateway port
(or --port). Refuses to overwrite an existing agent. When the roster
grows past one agent with the plane disabled, the enable-plane
advisory prints; when existing agents hold implicitly allocated ports,
the shift warning names them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetAdd(cmd, args[0], port, agentName, baseTag, telegram)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "explicit gateway port (default: next free from the base)")
	cmd.Flags().StringVar(&agentName, "agent-name", "", "display name (default: derived from <name>)")
	cmd.Flags().StringVar(&baseTag, "base-tag", scaffold.DefaultBaseTag, "base image tag the Dockerfile pins")
	cmd.Flags().BoolVar(&telegram, "telegram", true, "wire the telegram channel in spec.json")
	return cmd
}

func runFleetAdd(cmd *cobra.Command, name string, port int, agentName, baseTag string, telegram bool) error {
	out := cmd.OutOrStdout()
	root, err := resolveFleetRoot()
	if err != nil {
		return err
	}
	m, err := fleet.LoadManifest(root)
	if err != nil {
		return err
	}
	if _, exists := m.Agents[name]; exists {
		return fmt.Errorf("agent %q is already registered in %s", name, fleet.ManifestName)
	}
	if scaffold.ComposeProject(name) != name {
		return fmt.Errorf("agent name %q must be a valid directory/compose name (lowercase letters, digits, - _)", name)
	}

	if port == 0 {
		port = nextFreePort(m)
	}
	if agentName == "" {
		agentName = scaffold.DefaultAgentName(name)
	}
	planeShared := m.Plane.Enabled && m.Plane.LiteLLM == fleet.LiteLLMShared
	cfg := scaffold.Config{
		ProjectName:   name,
		AgentName:     agentName,
		BaseTag:       baseTag,
		Model:         scaffold.DefaultModel,
		GatewayPort:   port,
		Telegram:      telegram,
		TargetDir:     root,
		SharedLiteLLM: planeShared,
	}
	created, err := scaffold.Agent(cfg, name)
	if err != nil {
		return err
	}
	if err := insertFleetAgent(m, name, port); err != nil {
		return err
	}

	for _, rel := range created {
		fmt.Fprintln(out, "wrote   "+rel)
	}
	fmt.Fprintf(out, "added   %s (gateway port %d)\n", name, port)
	fmt.Fprintf(out, "next:   cd %s && agentctl secrets init && agentctl doctor && agentctl deploy\n",
		filepath.Join(fleet.AgentsDir, name))

	// Implicitly allocated ports shift when the sorted roster grows in
	// front of them: name every agent whose port would move so the
	// operator can pin before the next verb re-renders envelopes.
	shifted := agentsWhoseImplicitPortShifts(m, name)
	for _, s := range shifted {
		fmt.Fprintf(out, "note:   agents.%s holds an implicit port — adding %s re-sorts the allocation; pin `gateway_port:` to freeze it\n", s, name)
	}
	if len(m.Agents)+1 >= 2 && !m.Plane.Enabled {
		fmt.Fprintf(out, "note:   %d agents with plane disabled — enable the plane (shared LiteLLM + observability) or accept per-agent sidecars\n", len(m.Agents)+1)
	}
	return nil
}

// nextFreePort walks upward from the manifest base, skipping every
// port the current roster claims (explicit or allocated).
func nextFreePort(m *fleet.Manifest) int {
	taken := map[int]bool{}
	for _, name := range m.AgentNames() {
		taken[m.Agents[name].GatewayPort] = true
	}
	port := m.Plane.GatewayBasePort
	for taken[port] {
		port++
	}
	return port
}

// insertFleetAgent appends a registry entry to fleet.yaml's agents
// block (quoted key, explicit port), preserving comments and order.
func insertFleetAgent(m *fleet.Manifest, name string, port int) error {
	entry := fmt.Sprintf("  %q:\n    gateway_port: %d\n", name, port)
	data, err := os.ReadFile(m.Path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", m.Path, err)
	}
	lines := strings.Split(string(data), "\n")
	// Find the end of the agents block: the last line still inside it,
	// walking from the block header past every indented or blank line.
	start := -1
	for i, ln := range lines {
		if strings.TrimRight(ln, " ") == "agents:" {
			start = i
			break
		}
	}
	if start == -1 {
		return fmt.Errorf("no agents: block found in %s", m.Path)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		ln := lines[i]
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if !strings.HasPrefix(ln, " ") && !strings.HasPrefix(ln, "#") {
			end = i
			break
		}
	}
	out := make([]string, 0, len(lines)+2)
	out = append(out, lines[:end]...)
	out = append(out, entry)
	out = append(out, lines[end:]...)
	return os.WriteFile(m.Path, []byte(strings.Join(out, "\n")), 0o644)
}

// agentsWhoseImplicitPortShifts recomputes the sorted allocation with
// the new member and names every existing implicitly-allocated agent
// whose port would move.
func agentsWhoseImplicitPortShifts(m *fleet.Manifest, added string) []string {
	var shifted []string
	taken := map[int]string{}
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		if entry.ExplicitPort {
			taken[entry.GatewayPort] = name
		}
	}
	roster := append(m.AgentNames(), added)
	// Re-derive the sorted-name allocation exactly as the loader does.
	for i := 0; i < len(roster); i++ {
		for j := i + 1; j < len(roster); j++ {
			if roster[j] < roster[i] {
				roster[i], roster[j] = roster[j], roster[i]
			}
		}
	}
	candidate := m.Plane.GatewayBasePort
	for _, name := range roster {
		if name == added {
			for {
				if _, clash := taken[candidate]; clash {
					candidate++
					continue
				}
				break
			}
			candidate++
			continue
		}
		entry := m.Agents[name]
		if entry.ExplicitPort {
			continue
		}
		for {
			if _, clash := taken[candidate]; clash {
				candidate++
				continue
			}
			break
		}
		if candidate != entry.GatewayPort {
			shifted = append(shifted, name)
		}
		taken[candidate] = name
		candidate++
	}
	return shifted
}
