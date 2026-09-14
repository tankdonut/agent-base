package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// resolvedProject is what every project-scoped verb operates on: the
// agent directory (cwd after resolution) plus, in fleet repos, the
// manifest and the resolved agent key.
type resolvedProject struct {
	Root     string // the AGENT directory (fleet) — compose/spec paths are relative to it
	Manifest *fleet.Manifest
	Agent    string // registry key; "" never happens in fleet repos
}

// resolveProject locates the enclosing agent project: fleet root →
// agent selection (cwd scope, fleet-of-one default, legacy hard-error)
// → resolveAgentIn.
func resolveProject() (*resolvedProject, error) {
	fleetRoot, err := fleet.FindRoot(".")
	if err != nil {
		if _, legacy := project.FindProjectRoot("."); legacy == nil {
			return nil, fmt.Errorf("legacy single-agent layout (root-level %s) — run `agentctl migrate` to adopt the fleet.yaml structure", project.ProjectMarker)
		}
		return nil, err
	}
	m, err := fleet.LoadManifest(fleetRoot)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(fleetRoot, ConfigName)); err == nil {
		return nil, fmt.Errorf("%s is retired in fleet repos — fold its settings into fleet.yaml and delete it", ConfigName)
	}
	agent, err := targetAgent(m)
	if err != nil {
		return nil, err
	}
	return resolveAgentIn(m, agent)
}

// resolveAgentIn resolves one registered agent against its manifest
// entry: guards the authored-compose opt-out, (re)materializes the
// rendered envelope, and chdirs to the agent directory so every
// downstream consumer sees agent-dir-relative paths.
func resolveAgentIn(m *fleet.Manifest, name string) (*resolvedProject, error) {
	entry, ok := m.Agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %q is not registered in %s", name, fleet.ManifestName)
	}
	if entry.ComposeFile != "" {
		return nil, fmt.Errorf("agents.%s uses an authored compose_file (%s) — fleet verbs drive the rendered envelope; authored support is tracked for a later phase", name, entry.ComposeFile)
	}
	if _, err := fleet.MaterializeAgentCompose(m, name); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(entry.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(abs); err != nil {
		return nil, err
	}
	return &resolvedProject{Root: abs, Manifest: m, Agent: name}, nil
}

// targetAgent picks the roster entry: cwd under agents/<name> scopes to
// that agent (registered, or a hard error when the marker exists
// unregistered); otherwise a fleet of one defaults to its only agent
// and anything larger demands an agent-scoped cwd.
func targetAgent(m *fleet.Manifest) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(m.Root, cwd); err == nil && rel != "." {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) >= 2 && parts[0] == fleet.AgentsDir {
			key := parts[1]
			if _, ok := m.Agents[key]; ok {
				return key, nil
			}
			if _, err := os.Stat(filepath.Join(m.Root, fleet.AgentsDir, key, project.ProjectMarker)); err == nil {
				return "", fmt.Errorf("agents/%s carries %s but is not registered in fleet.yaml — register it (renames are hard errors, not silent fleet shrinkage)", key, project.ProjectMarker)
			}
		}
	}
	if len(m.Agents) == 1 {
		for key := range m.Agents {
			return key, nil
		}
	}
	return "", fmt.Errorf("%d agents registered in fleet.yaml — run from inside agents/<name> to scope a single agent", len(m.Agents))
}

// chdirProject is resolveProject for callers that only need the root.
func chdirProject() (string, error) {
	rp, err := resolveProject()
	if err != nil {
		return "", err
	}
	return rp.Root, nil
}

// fleetConfig builds the effective Config for a resolved fleet agent:
// manifest platform routing and compose engine defaults, overlaid by
// the AGENTCTL_* env contract (env wins, matching .agentctl.yaml
// precedence before retirement).
func fleetConfig(rp *resolvedProject) (Config, error) {
	cfg := Config{Platform: "compose", Namespaces: map[string]map[string]any{}}
	if rp.Manifest != nil {
		if p := rp.Manifest.Agents[rp.Agent].Platform; p != "" {
			cfg.Platform = p
		}
		if e := rp.Manifest.Defaults.ComposeEngine; e != "" {
			cfg.Namespaces["compose"] = map[string]any{"engine": e}
		}
	}
	if err := applyConfigEnv(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// loadProjectPlatformFor resolves everything a release verb needs for
// one registered agent of an already-loaded manifest: the agent root
// (chdir into it), the effective config, the derived Deployment, and
// the constructed platform.
func loadProjectPlatformFor(m *fleet.Manifest, name string) (string, platform.Platform, platform.Deployment, error) {
	rp, err := resolveAgentIn(m, name)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	cfg, err := fleetConfig(rp)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	d, err := platform.Derive(rp.Root)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	p, err := forPlatform(cfg.Platform, newRunner(), cfg.Namespaces)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	return rp.Root, p, d, nil
}

// loadProjectPlatform is the cwd-resolved variant for the single-agent
// verbs. Failing early here means every verb fails the same way with
// the same hints.
func loadProjectPlatform() (string, platform.Platform, platform.Deployment, error) {
	rp, err := resolveProject()
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	return loadProjectPlatformFor(rp.Manifest, rp.Agent)
}

// newValidateCmd runs the base image's --validate-spec gate.
func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate spec + automations via the base image (--validate-spec)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			engine, err := resolveComposeEngine()
			if err != nil {
				return err
			}
			return compose.Validate(newRunner(), engine, root)
		},
	}
}

// resolveComposeEngine maps the configured compose.engine preference to
// a binary — a local-dev concern only; release verbs get their engine
// from the compose adapter itself.
func resolveComposeEngine() (string, error) {
	rp, err := resolveProject()
	if err != nil {
		return "", err
	}
	cfg, err := fleetConfig(rp)
	if err != nil {
		return "", err
	}
	return process.ResolveEngine(cfg.ComposeEnginePref(), newRunner())
}
