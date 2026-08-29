package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// baseImagePrefix identifies the project Dockerfile's base image line.
const baseImagePrefix = "FROM ghcr.io/tankdonut/agent-base:"

// BaseTagFromDockerfile extracts the base image tag from the project
// Dockerfile: everything after the colon on the
// `FROM ghcr.io/tankdonut/agent-base:<tag>` line, which correctly
// carries a possible @sha256 digest suffix.
func BaseTagFromDockerfile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, baseImagePrefix) {
			continue
		}
		tag := strings.TrimPrefix(trimmed, baseImagePrefix)
		if tag == "" {
			return "", fmt.Errorf("%s: empty base image tag", path)
		}
		return tag, nil
	}
	return "", fmt.Errorf("%s: no `FROM ghcr.io/tankdonut/agent-base:<tag>` line found", path)
}

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
func Validate(r Runner, engine, root string) error {
	if r == nil {
		return errNilRunner
	}
	tag, err := BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
	if err != nil {
		return err
	}
	info, err := ReadSpec(filepath.Join(root, "agent", "spec.json"))
	if err != nil {
		return err
	}
	envExample := filepath.Join(root, "agent", ".env.example")
	if _, err := os.Stat(envExample); err != nil {
		return fmt.Errorf("agent/.env.example not found — run `agentctl init`")
	}

	argv := []string{"run", "--rm", "--env-file", "agent/.env.example"}
	dummies := append([]string{}, info.EnvRefs...)
	if info.RequiresZAIKey() && !contains(dummies, "ZAI_API_KEY") {
		dummies = append(dummies, "ZAI_API_KEY")
	}
	for _, name := range dummies {
		argv = append(argv, "-e", name+"=dummy")
	}
	argv = append(argv, "ghcr.io/tankdonut/agent-base:"+tag, "--validate-spec")
	return runArgv(r, nil, append([]string{engine}, argv...)...)
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
