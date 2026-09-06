package cli

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/lifecycle"
	"github.com/tankdonut/agent-base/internal/platform"
)

// chdirProject locates the enclosing agent project, chdirs to its root
// (compose and validate use root-relative paths), and returns the
// absolute root.
func chdirProject() (string, error) {
	root, err := lifecycle.FindProjectRoot(".")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := os.Chdir(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// loadProjectPlatform resolves everything the release verbs need: the
// project root (chdir into it), the loaded config, the derived
// Deployment, and the constructed pinned platform. Failing early here
// means every verb fails the same way with the same hints.
func loadProjectPlatform() (string, platform.Platform, platform.Deployment, error) {
	root, err := chdirProject()
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	d, err := platform.Derive(root)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	p, err := forPlatform(cfg.Platform, newRunner(), cfg.Namespaces)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	return root, p, d, nil
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
			return lifecycle.Validate(newRunner(), engine, root)
		},
	}
}

// resolveComposeEngine maps the configured compose.engine preference to
// a binary — a local-dev concern only; release verbs get their engine
// from the compose adapter itself.
func resolveComposeEngine() (string, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return "", err
	}
	return lifecycle.ResolveEngine(cfg.ComposeEnginePref(), newRunner())
}
