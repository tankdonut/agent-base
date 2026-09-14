// migrateverb.go hosts `agentctl migrate`: the one-command carry-over
// from the legacy single-agent layout (root-level agent/, compose.yml,
// .agentctl.yaml) to the canonical fleet shape (fleet.yaml +
// agents/<key>/). In-place and file-move based — the operator reviews
// and commits; nothing touches remotes or volumes.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/project"
	"github.com/tankdonut/agent-base/internal/scaffold"
)

// agentScopedDirs are the legacy root-level trees that move under
// agents/<key>/ wholesale.
var agentScopedDirs = []string{"litellm", "knowledge", "deploy"}

// rewriteDockerfileCopies strips the legacy agent/ prefix from COPY
// source paths now that the agent dir IS the build context root.
func rewriteDockerfileCopies(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}
	rewritten := strings.ReplaceAll(string(data), " agent/spec.json ", " spec.json ")
	rewritten = strings.ReplaceAll(rewritten, " agent/automations/", " automations/")
	rewritten = strings.ReplaceAll(rewritten, " agent/workspace/", " workspace/")
	rewritten = strings.ReplaceAll(rewritten, " agent/skills/", " skills/")
	if rewritten != string(data) {
		if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

// portDefaultRe recovers the gateway port a legacy compose.yml carried
// as the AGENT_GATEWAY_PORT interpolation default.
var portDefaultRe = regexp.MustCompile(`\$\{AGENT_GATEWAY_PORT:-(\d+)\}`)

func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Carry a legacy single-agent repo into the fleet.yaml structure",
		Long: `In-place restructure: agent content moves under agents/<key>/,
the deployment envelope is dropped (agentctl renders it from
fleet.yaml on every verb), and .agentctl.yaml settings fold into
the synthesized manifest. Review with git status, then commit.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrateTo(cmd.OutOrStdout())
		},
	}
}

// legacyConfig is the folded .agentctl.yaml surface migrate understands.
type legacyConfig struct {
	Platform    string
	GatewayPort int
	Engine      string
	FlyApp      string
	FlyRegion   string
}

func runMigrateTo(w interface{ Write([]byte) (int, error) }) error {
	root, err := project.FindProjectRoot(".")
	if err != nil {
		if _, fleetErr := fleet.FindRoot("."); fleetErr == nil {
			return fmt.Errorf("this repo already carries a %s — nothing to migrate", fleet.ManifestName)
		}
		return err
	}
	if _, err := os.Stat(filepath.Join(root, fleet.ManifestName)); err == nil {
		return fmt.Errorf("%s already exists at %s — nothing to migrate", fleet.ManifestName, root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	key := scaffold.ComposeProject(filepath.Base(abs))
	agentDir := filepath.Join(abs, fleet.AgentsDir, key)
	if _, err := os.Stat(agentDir); err == nil {
		return fmt.Errorf("%s already exists — refusing to move the legacy tree into it", agentDir)
	}

	legacy := readLegacyConfig(filepath.Join(abs, ConfigName))
	port := legacy.GatewayPort
	if port == 0 {
		port = recoverPortFromCompose(filepath.Join(abs, "compose.yml"))
	}

	var moves []string
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", agentDir, err)
	}
	// The legacy agent/ wrapper flattens: its children ARE the agent
	// directory now (the parent already names the agent).
	if fi, err := os.Stat(filepath.Join(abs, "agent")); err == nil && fi.IsDir() {
		entries, err := os.ReadDir(filepath.Join(abs, "agent"))
		if err != nil {
			return fmt.Errorf("reading the legacy agent/ tree: %w", err)
		}
		for _, e := range entries {
			src := filepath.Join(abs, "agent", e.Name())
			dst := filepath.Join(agentDir, e.Name())
			if _, err := os.Lstat(dst); err == nil {
				return fmt.Errorf("refusing to overwrite %s — resolve manually and re-run", dst)
			}
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("moving %s: %w", e.Name(), err)
			}
			moves = append(moves, fleet.AgentsDir+"/"+key+"/"+e.Name())
		}
		if err := os.Remove(filepath.Join(abs, "agent")); err != nil {
			return fmt.Errorf("removing the emptied legacy agent/ wrapper: %w", err)
		}
	}
	for _, name := range []string{"litellm", "knowledge", "deploy"} {
		src := filepath.Join(abs, name)
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			continue
		}
		if err := os.Rename(src, filepath.Join(agentDir, name)); err != nil {
			return fmt.Errorf("moving %s: %w", name, err)
		}
		moves = append(moves, fleet.AgentsDir+"/"+key+"/"+name+"/")
	}
	if _, err := os.Stat(filepath.Join(abs, "compose.dev.yml")); err == nil {
		if err := os.Rename(filepath.Join(abs, "compose.dev.yml"), filepath.Join(agentDir, "compose.dev.yml")); err != nil {
			return fmt.Errorf("moving compose.dev.yml: %w", err)
		}
		moves = append(moves, fleet.AgentsDir+"/"+key+"/compose.dev.yml")
	}
	// The moved Dockerfile's COPY sources drop the agent/ prefix.
	if err := rewriteDockerfileCopies(filepath.Join(agentDir, "Dockerfile")); err != nil {
		return err
	}

	// The envelope is rendered state now; the manifest replaces it.
	if err := os.Remove(filepath.Join(abs, "compose.yml")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the legacy compose.yml: %w", err)
	}
	if legacy.Platform != "" || legacy.Engine != "" || legacy.FlyApp != "" {
		if err := os.Remove(filepath.Join(abs, ConfigName)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing the folded %s: %w", ConfigName, err)
		}
	}

	manifest := &strings.Builder{}
	fmt.Fprintf(manifest, "# Fleet manifest (migrated from the legacy single-agent layout).\n")
	fmt.Fprintf(manifest, "# The compose envelope is RENDERED from this file — never authored.\n")
	fmt.Fprintf(manifest, "agents:\n  %q:\n", key)
	if legacy.Platform != "" {
		fmt.Fprintf(manifest, "    platform: %s\n", legacy.Platform)
	}
	fmt.Fprintf(manifest, "    gateway_port: %d\n", port)
	if legacy.Engine != "" {
		fmt.Fprintf(manifest, "    # engine: %s (was compose.engine; fleet default goes under defaults.compose.engine)\n", legacy.Engine)
	}
	if legacy.FlyApp != "" {
		manifest.WriteString("    fly:\n")
		fmt.Fprintf(manifest, "      app: %s\n", legacy.FlyApp)
		if legacy.FlyRegion != "" {
			fmt.Fprintf(manifest, "      region: %s\n", legacy.FlyRegion)
		}
	}
	manifestPath := filepath.Join(abs, fleet.ManifestName)
	if err := os.WriteFile(manifestPath, []byte(manifest.String()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", manifestPath, err)
	}

	if err := ensureRenderIgnore(abs); err != nil {
		return err
	}

	sort.Strings(moves)
	for _, m := range moves {
		fmt.Fprintln(w, "moved   "+m)
	}
	fmt.Fprintln(w, "wrote   "+fleet.ManifestName)
	fmt.Fprintln(w, "removed compose.yml (rendered from the manifest from now on)")
	fmt.Fprintf(w, "\nnext: git status && git add -A, review, commit; then `agentctl fleet check` and `agentctl doctor`\n")
	fmt.Fprintf(w, "note: README.md / AGENTS.md structure tables predate the fleet layout — refresh them against a fresh `agentctl init` tree\n")
	return nil
}

// readLegacyConfig folds the retired .agentctl.yaml into the fields
// migrate carries forward. Malformed content fails the migration
// rather than silently dropping settings.
func readLegacyConfig(path string) legacyConfig {
	var out legacyConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return out
	}
	if p, ok := raw["platform"].(string); ok {
		out.Platform = p
	}
	if comp, ok := raw["compose"].(map[string]any); ok {
		if n, ok := comp["gateway_port"].(int); ok {
			out.GatewayPort = n
		}
		if e, ok := comp["engine"].(string); ok {
			out.Engine = e
		}
	}
	if f, ok := raw["fly"].(map[string]any); ok {
		out.FlyApp, _ = f["app"].(string)
		out.FlyRegion, _ = f["region"].(string)
	}
	return out
}

// recoverPortFromCompose pulls the AGENT_GATEWAY_PORT interpolation
// default out of a legacy envelope; 0 when absent.
func recoverPortFromCompose(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	if m := portDefaultRe.FindSubmatch(data); m != nil {
		var port int
		fmt.Sscanf(string(m[1]), "%d", &port)
		return port
	}
	return 0
}

// ensureRenderIgnore appends the agentctl-namespace ignore patterns,
// creating .gitignore when the repo has none. plane/ covers both the
// rendered plane stack and its authored secrets (plane/.env).
func ensureRenderIgnore(root string) error {
	for _, pattern := range []string{"agents/*/compose.yml", "agents/*/.agentctl/", "plane/"} {
		if err := appendIgnorePattern(root, pattern); err != nil {
			return err
		}
	}
	return nil
}

// appendIgnorePattern adds one pattern to .gitignore (creating the
// file when absent), skipping patterns already present.
func appendIgnorePattern(root, pattern string) error {
	path := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, []byte(pattern+"\n")...)
	return os.WriteFile(path, data, 0o644)
}
