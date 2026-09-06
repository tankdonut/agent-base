package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

// Deployment is the platform-neutral view of one agent project,
// DERIVED at invoke time from repo-owned files: the agent-base tag from
// agent/Dockerfile, the env surface from agent/spec.json, the set keys
// from agent/.env. It is never persisted and never a parallel source of
// truth — the repo is. It holds no secret VALUES: EnvKeys are names
// only, so a Deployment is safe to print.
type Deployment struct {
	Project  string   // compose project name (from compose.yml `name:`, else sanitized root basename)
	Root     string   // absolute project root
	BaseTag  string   // agent-base date tag from agent/Dockerfile (digest suffix allowed)
	EnvKeys  []string // sorted KEY names set in agent/.env; nil when absent
	SpecRefs []string // sorted {env:NAME} refs in agent/spec.json (unguarded subset = required)
}

// composeNameRe reads the pinned project name from compose.yml without
// a full YAML parse: the scaffold renders it top-level as `name: x`.
var composeNameRe = regexp.MustCompile(`(?m)^name:\s*(\S+)\s*$`)

// Derive builds the Deployment for the repo at root. Fail-closed: a
// Dockerfile without a pinned base line or an unparsable spec aborts —
// deployment must not proceed from a partial read of the project.
func Derive(root string) (Deployment, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Deployment{}, fmt.Errorf("resolving %s: %w", root, err)
	}
	tag, err := lifecycle.BaseTagFromDockerfile(filepath.Join(abs, "agent", "Dockerfile"))
	if err != nil {
		return Deployment{}, err
	}
	info, err := lifecycle.ReadSpec(filepath.Join(abs, "agent", "spec.json"))
	if err != nil {
		return Deployment{}, err
	}
	keys, err := lifecycle.EnvKeyNames(abs)
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{
		Project:  projectName(abs),
		Root:     abs,
		BaseTag:  tag,
		EnvKeys:  keys,
		SpecRefs: info.EnvRefs,
	}, nil
}

// projectName resolves the compose project name from compose.yml's
// pinned `name:` (immune to directory renames and worktree checkouts);
// a missing file or missing key falls back to the sanitized basename,
// matching scaffold.ComposeProject.
func projectName(absRoot string) string {
	if data, err := os.ReadFile(filepath.Join(absRoot, "compose.yml")); err == nil {
		if m := composeNameRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return sanitizeName(filepath.Base(absRoot))
}

// sanitizeName mirrors scaffold.ComposeProject: lowercased, dots mapped
// to dashes (compose project names are [a-z0-9][a-z0-9_-]*).
func sanitizeName(base string) string {
	return strings.ReplaceAll(strings.ToLower(base), ".", "-")
}
