package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
)

// ProjectMarker is the file that identifies a scaffolded agent project.
const ProjectMarker = "agent/spec.json"

// FindProjectRoot walks up from start until it finds a directory
// containing agent/spec.json, and returns it as an absolute path. The
// error names the missing marker when no ancestor qualifies.
func FindProjectRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", start, err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ProjectMarker)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found in %s or any parent — run inside a scaffolded agent project (see `agentctl init`)", ProjectMarker, start)
		}
		dir = parent
	}
}
