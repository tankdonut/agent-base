// agent.go renders ONE agent's scoped content into an existing fleet
// repo — the add-an-agent half of `fleet add`, kept in the scaffold
// leaf next to the templates it shares with init.
package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Agent emits the agent-scoped template subset (everything except the
// repo files: spec, Dockerfile, env example, automations, skills,
// workspace, knowledge, litellm, dev overlay) under
// <target>/agents/<key>/. It refuses when the agent directory already
// carries a spec.json — adding never clobbers an existing agent — and
// returns the created paths relative to the target. When the fleet's
// plane provides the shared proxy (cfg.SharedLiteLLM), the agent-local
// litellm/ tree is skipped: the plane owns the proxy and the agent
// gets a minted virtual key instead.
func Agent(cfg Config, key string) ([]string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if key == "" || ComposeProject(key) != key {
		return nil, fmt.Errorf("agent key %q is not already a valid directory/compose name (lowercase letters, digits, - _)", key)
	}
	agentDir := filepath.Join(cfg.TargetDir, filepath.FromSlash(AgentsDirPrefix()+key))
	if _, err := os.Stat(filepath.Join(agentDir, "spec.json")); err == nil {
		return nil, fmt.Errorf("%s already carries a spec.json — refusing to overwrite an existing agent", agentDir)
	}
	data := templateData{
		ProjectName:    cfg.ProjectName,
		ComposeProject: key,
		AgentKey:       key,
		AgentName:      cfg.AgentName,
		BaseTag:        cfg.BaseTag,
		Model:          cfg.Model,
		GatewayPort:    cfg.GatewayPort,
		Telegram:       cfg.Telegram,
	}
	tmplFS := FS()
	paths, err := Paths()
	if err != nil {
		return nil, err
	}
	created := []string{}
	for _, rel := range paths {
		if repoFiles[rel] {
			continue
		}
		if cfg.SharedLiteLLM && strings.HasPrefix(rel, "litellm/") {
			continue
		}
		out := OutputPath(rel, key)
		if err := renderFile(tmplFS, cfg.TargetDir, rel, out, data); err != nil {
			return nil, fmt.Errorf("%w — partial agent tree left in %s: remove %s and re-run", err, agentDir, agentDir)
		}
		created = append(created, out)
	}
	return created, nil
}

// AgentsDirPrefix is the trailing-slash form of the fleet agents
// directory, for callers building their own paths.
func AgentsDirPrefix() string {
	return agentsDir + "/"
}
