package compose

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// Validate runs the base image's --validate-spec mode against the
// project's spec and automations without touching any volume:
//
//	<engine> run --rm --env-file agent/.env.example \
//	  [-e NAME=dummy ...] ghcr.io/tankdonut/agent-base:<tag> --validate-spec
//
// Every {env:NAME} ref gets a dummy -e value so resolution never fails
// on the example file's commented-out entries; ZAI_API_KEY is added
// when the auth provider load-gates on it. Paths are relative, so
// callers must run with the project root as cwd.
func Validate(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	tag, err := project.BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
	if err != nil {
		return err
	}
	info, err := project.ReadSpec(filepath.Join(root, "agent", "spec.json"))
	if err != nil {
		return err
	}
	envExample := filepath.Join(root, "agent", ".env.example")
	if _, err := os.Stat(envExample); err != nil {
		return fmt.Errorf("agent/.env.example not found — run `agentctl init`")
	}

	argv := []string{"run", "--rm", "--env-file", "agent/.env.example"}
	dummies := append([]string{}, info.EnvRefs...)
	if k := info.AuthEnvKey(); k != "" && !contains(dummies, k) {
		dummies = append(dummies, k)
	}
	for _, name := range dummies {
		argv = append(argv, "-e", name+"=dummy")
	}
	argv = append(argv, "ghcr.io/tankdonut/agent-base:"+tag, "--validate-spec")
	return process.RunArgv(r, nil, append([]string{engine}, argv...)...)
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
