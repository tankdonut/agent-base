// This file embeds the scaffold template tree (tmpl/) and exposes its
// shape: one "<output-path>.tmpl" file per generated file. FS, Mode,
// and Paths are the rendering surface scaffold.Run consumes.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed all:tmpl
var embedded embed.FS

// FS returns the embedded template tree. Paths are slash-separated and
// rooted at "tmpl".
func FS() fs.FS { return embedded }

// modes is the file-mode table for generated paths. Directories are
// always 0755; make.sh is the only executable file.
var modes = map[string]fs.FileMode{
	"make.sh": 0o755,
}

// defaultMode is the mode for every generated file not in the table.
const defaultMode = fs.FileMode(0o644)

// Mode returns the output file mode for a generated path.
func Mode(path string) fs.FileMode {
	if m, ok := modes[path]; ok {
		return m
	}
	return defaultMode
}

// Paths returns every generated output path (the tmpl tree minus the
// ".tmpl" suffix), sorted. It fails loudly on stray files so a template
// added without the suffix cannot silently ship.
//
// Paths are SOURCE-relative: the fleet-of-one layout renders repo files
// at the root and everything else under agents/<key>/ — see
// OutputPath. Generated-location lists come from Run.
func Paths() ([]string, error) {
	var out []string
	err := fs.WalkDir(embedded, "tmpl", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, "tmpl/")
		if !strings.HasSuffix(rel, ".tmpl") {
			return fmt.Errorf("templates: stray non-template file: %s", rel)
		}
		out = append(out, strings.TrimSuffix(rel, ".tmpl"))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// agentsDir is the fleet per-agent directory. It must equal
// fleet.AgentsDir; scaffold is a self-contained leaf (it may not import
// internal/fleet), so cli tests assert the equality instead.
const agentsDir = "agents"

// repoFiles lists template sources that render to the repo root; every
// other source renders under agents/<key>/ (agent content, the dev
// overlay, litellm, knowledge).
var repoFiles = map[string]bool{
	".gitignore":               true,
	".markdownlint-cli2.yaml":  true,
	".pre-commit-config.yaml":  true,
	".github/workflows/ci.yml": true,
	"AGENTS.md":                true,
	"README.md":                true,
	"fleet.yaml":               true,
	"make.sh":                  true,
	"renovate.json":            true,
}

// OutputPath maps a template source path to its generated location for
// the given agent key: repo files at the root, agent-scoped content
// under agents/<key>/.
func OutputPath(rel, agentKey string) string {
	if repoFiles[rel] {
		return rel
	}
	return agentsDir + "/" + agentKey + "/" + rel
}
