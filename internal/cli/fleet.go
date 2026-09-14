// fleet.go hosts the fleet verbs: manifest-driven operations over
// every agent registered in fleet.yaml. P0 surface is read-only —
// roster listing and the check gate (marker presence, port ownership,
// orphan + plane advisories, rendered-artifact staleness). The verb
// fan-out (deploy/status/...) arrives with P1 and reuses the same
// root-resolution path.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// resolveFleetRoot finds the fleet root, translating the two failure
// modes into actionable errors: outside any repo, and inside a legacy
// single-agent layout (root-level agent/spec.json, pre-fleet.yaml).
func resolveFleetRoot() (string, error) {
	root, err := fleet.FindRoot(".")
	if err == nil {
		return root, nil
	}
	if _, legacy := project.FindProjectRoot("."); legacy == nil {
		return "", fmt.Errorf("this is a legacy single-agent layout (root-level %s) — run `agentctl migrate` to adopt the fleet.yaml structure", project.ProjectMarker)
	}
	return "", err
}

func loadFleet() (*fleet.Manifest, error) {
	root, err := resolveFleetRoot()
	if err != nil {
		return nil, err
	}
	return fleet.LoadManifest(root)
}

func newFleetCmd() *cobra.Command {
	fleetCmd := &cobra.Command{
		Use:   "fleet",
		Short: "Operate the agent fleet declared in fleet.yaml",
		Long: `Fleet operations over the manifest: every repo is a fleet
(a single-agent repo is a fleet of one). agents/<name>/ directories
carry authored image content; deployment envelopes are rendered from
fleet.yaml — never authored.`,
	}
	fleetCmd.AddCommand(newFleetLsCmd())
	fleetCmd.AddCommand(newFleetCheckCmd())
	fleetCmd.AddCommand(newFleetAddCmd())
	fleetCmd.AddCommand(newFleetVerbCmds()...)
	return fleetCmd
}

func newFleetLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the agents (and services) registered in fleet.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadFleet()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-16s %-9s %-6s %-8s %s\n", "AGENT", "PLATFORM", "PORT", "LITELLM", "DIR")
			for _, name := range m.AgentNames() {
				entry := m.Agents[name]
				platform := entry.Platform
				if platform == "" {
					platform = "compose"
				}
				rel, err := filepath.Rel(m.Root, entry.Dir)
				if err != nil {
					rel = entry.Dir
				}
				fmt.Fprintf(out, "%-16s %-9s %-6d %-8s %s\n", name, platform, entry.GatewayPort, entry.LiteLLMUsed, rel)
			}
			for _, name := range m.ServiceNames() {
				entry := m.Services[name]
				rel, err := filepath.Rel(m.Root, entry.Dir)
				if err != nil {
					rel = entry.Dir
				}
				fmt.Fprintf(out, "%-16s %-9s %-6s %-8s %s\n", "svc:"+name, "-", "-", "-", rel)
			}
			return nil
		},
	}
}

// fleetFinding is one check result; severity drives the exit code.
type fleetFinding struct {
	severity string // "FAIL" | "WARN"
	line     string
}

func newFleetCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Validate the fleet manifest against the repo (markers, ports, drift)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			m, err := loadFleet()
			if err != nil {
				return err
			}
			var findings []fleetFinding
			findings = append(findings, checkAgentMarkers(m)...)
			findings = append(findings, checkServiceMarkers(m)...)
			findings = append(findings, checkEnvPortOwnership(m)...)
			findings = append(findings, checkAgentPlatforms(m)...)
			findings = append(findings, checkOrphanAgentDirs(m)...)
			findings = append(findings, checkRetiredAgentctlConfig(m)...)
			findings = append(findings, checkPlaneAdvisory(m)...)
			findings = append(findings, checkRenderedArtifactDrift(m)...)
			findings = append(findings, checkRunningPortDrift(m)...)

			errors := 0
			warnings := 0
			for _, f := range findings {
				fmt.Fprintf(out, "%s: %s\n", f.severity, f.line)
				if f.severity == "FAIL" {
					errors++
				} else {
					warnings++
				}
			}
			plane := "off"
			if m.Plane.Enabled {
				plane = m.Plane.Name
			}
			fmt.Fprintf(out, "%d agent(s), %d service(s), plane %s — %d error(s), %d warning(s)\n",
				len(m.Agents), len(m.Services), plane, errors, warnings)
			if errors > 0 {
				return fmt.Errorf("fleet check found %d error(s)", errors)
			}
			return nil
		},
	}
}

func checkAgentMarkers(m *fleet.Manifest) []fleetFinding {
	var out []fleetFinding
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		if _, err := os.Stat(filepath.Join(entry.Dir, "spec.json")); err != nil {
			out = append(out, fleetFinding{"FAIL", fmt.Sprintf("agents.%s: %s missing — every registered agent directory needs a spec.json", name, filepath.Join("agents", name, "spec.json"))})
		}
	}
	return out
}

func checkServiceMarkers(m *fleet.Manifest) []fleetFinding {
	var out []fleetFinding
	for _, name := range m.ServiceNames() {
		entry := m.Services[name]
		if _, err := os.Stat(filepath.Join(entry.Dir, "compose.yml")); err != nil {
			out = append(out, fleetFinding{"FAIL", fmt.Sprintf("services.%s: compose.yml missing — fleet services own an authored compose file", name)})
		}
	}
	return out
}

// checkEnvPortOwnership enforces the manifest's port monopoly: an
// AGENT_GATEWAY_PORT line in agent/.env would win compose
// interpolation over the manifest allocation (stale-.env precedence
// inversion), so it is a hard error inside a fleet.
func checkEnvPortOwnership(m *fleet.Manifest) []fleetFinding {
	var out []fleetFinding
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		envPath := filepath.Join(entry.Dir, ".env")
		data, err := os.ReadFile(envPath)
		if err != nil {
			continue // missing .env is the secrets gate's error, not ours
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "AGENT_GATEWAY_PORT=") || strings.HasPrefix(trimmed, "export AGENT_GATEWAY_PORT=") {
				out = append(out, fleetFinding{"FAIL", fmt.Sprintf("agents.%s: agent/.env sets AGENT_GATEWAY_PORT — the fleet manifest owns gateway ports (agents.%s.gateway_port / plane.gateway_base_port)", name, name)})
				break
			}
		}
	}
	return out
}

func checkAgentPlatforms(m *fleet.Manifest) []fleetFinding {
	var out []fleetFinding
	for _, name := range m.AgentNames() {
		platform := m.Agents[name].Platform
		if platform == "" {
			continue // compose default
		}
		if _, known := platformRegistry[platform]; !known {
			out = append(out, fleetFinding{"FAIL", fmt.Sprintf("agents.%s.platform: unknown platform %q (available: %s)", name, platform, strings.Join(platformNames(), ", "))})
		}
	}
	return out
}

func checkOrphanAgentDirs(m *fleet.Manifest) []fleetFinding {
	var out []fleetFinding
	agentsDir := filepath.Join(m.Root, fleet.AgentsDir)
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return out
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, registered := m.Agents[e.Name()]; registered {
			continue
		}
		if _, err := os.Stat(filepath.Join(agentsDir, e.Name(), "spec.json")); err == nil {
			orphans = append(orphans, e.Name())
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		out = append(out, fleetFinding{"WARN", fmt.Sprintf("agents/%s carries spec.json but is not registered in fleet.yaml — add it or remove the directory (renames are hard errors, not silent fleet shrinkage)", name)})
	}
	return out
}

func checkRetiredAgentctlConfig(m *fleet.Manifest) []fleetFinding {
	if _, err := os.Stat(filepath.Join(m.Root, ConfigName)); err == nil {
		return []fleetFinding{{"WARN", fmt.Sprintf("%s is retired in fleet repos — fold its settings into fleet.yaml and delete it", ConfigName)}}
	}
	return nil
}

func checkPlaneAdvisory(m *fleet.Manifest) []fleetFinding {
	if len(m.Agents) >= 2 && !m.Plane.Enabled {
		return []fleetFinding{{"WARN", fmt.Sprintf("%d agents with plane disabled — enable the plane (shared LiteLLM + observability) or accept per-agent sidecars", len(m.Agents))}}
	}
	return nil
}

// checkRunningPortDrift compares each agent's manifest gateway port
// against what a RUNNING stack actually publishes (compose ps). The
// running config is the one thing verbs cannot regenerate: a manifest
// port edit after a deploy leaves the container on the old port until
// the next deploy — surface that as a warning. Engine-optional: no
// engine or no running stack means nothing to compare, silently.
func checkRunningPortDrift(m *fleet.Manifest) []fleetFinding {
	engine, err := process.ResolveEngine(m.Defaults.ComposeEngine, newRunner())
	if err != nil {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	defer func() { _ = os.Chdir(cwd) }()
	var out []fleetFinding
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		if err := os.Chdir(entry.Dir); err != nil {
			continue
		}
		data, err := compose.PsJSON(newRunner(), engine)
		if err != nil {
			continue
		}
		published := publishedHostPorts(data)
		if len(published) == 0 {
			continue
		}
		if !published[entry.GatewayPort] {
			out = append(out, fleetFinding{"WARN", fmt.Sprintf("agents.%s: the running stack publishes %v but the manifest allocates %d — re-run a fleet verb (or deploy) to converge, or fix the manifest", name, sortedKeys(published), entry.GatewayPort)})
		}
	}
	return out
}

// publishedHostPorts normalizes `compose ps --format json` across
// engines: docker compose reports Publishers[].PublishedPort; podman
// reports Ports[].host_port. Both shapes are accepted; anything
// unparsable yields an empty set (the caller skips).
func publishedHostPorts(data []byte) map[int]bool {
	ports := map[int]bool{}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return ports
	}
	for _, row := range rows {
		if pubs, ok := row["Publishers"].([]any); ok {
			for _, p := range pubs {
				if m, ok := p.(map[string]any); ok {
					if n, ok := m["PublishedPort"].(float64); ok && n > 0 {
						ports[int(n)] = true
					}
				}
			}
		}
		if tis, ok := row["Ports"].([]any); ok {
			for _, p := range tis {
				if m, ok := p.(map[string]any); ok {
					if n, ok := m["host_port"].(float64); ok && n > 0 {
						ports[int(n)] = true
					}
				}
			}
		}
	}
	return ports
}

func sortedKeys(m map[int]bool) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// checkRenderedArtifactDrift compares the on-disk rendered envelope
// against a fresh render: hand-edits and stale artifacts surface here
// as warnings (the artifact is derived state; the next verb
// regenerates it). Authored opt-outs are skipped.
func checkRenderedArtifactDrift(m *fleet.Manifest) []fleetFinding {
	var out []fleetFinding
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		if entry.ComposeFile != "" {
			continue
		}
		fresh, err := fleet.RenderAgentCompose(m, name)
		if err != nil {
			out = append(out, fleetFinding{"FAIL", fmt.Sprintf("agents.%s: rendering envelope: %v", name, err)})
			continue
		}
		path := filepath.Join(entry.Dir, fleet.RenderedComposeName)
		onDisk, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue // not yet materialized — the first verb writes it
		}
		if err != nil {
			out = append(out, fleetFinding{"FAIL", fmt.Sprintf("agents.%s: reading %s: %v", name, path, err)})
			continue
		}
		if !bytes.Equal(onDisk, fresh) {
			out = append(out, fleetFinding{"WARN", fmt.Sprintf("agents.%s: %s is stale or hand-edited — it differs from the fleet.yaml render and will be regenerated on the next fleet verb", name, fleet.RenderedComposeName)})
		}
	}
	return out
}
