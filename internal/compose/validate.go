package compose

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// Validate runs the base image's --validate-spec mode against the
// project's spec and automations — a read-only gate that mounts the
// inputs the image-side parse needs:
//
//	<engine> run --rm --security-opt label=disable \
//	  -v <root>/agent/spec.json:/opt/agent/spec.json:ro \
//	  -v <root>/agent/automations:/opt/agent/automations:ro \
//	  [-v <root>/agent/scripts:/opt/agent/scripts:ro — when shipped] \
//	  --env-file agent/.env.example \
//	  [-e NAME=dummy ...] ghcr.io/tankdonut/agent-base:<tag> --validate-spec
//
// Every {env:NAME} ref gets a dummy -e value so resolution never fails
// on the example file's commented-out entries; the auth-gated key is
// added when the auth choice load-gates on it. Mount sources are
// absolute (a bare relative source is a named volume to the engine);
// label=disable instead of :ro,Z so the gate never relabels repo files.
// Callers must run with the project root as cwd.
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
	automations := filepath.Join(root, "agent", "automations")
	if fi, err := os.Stat(automations); err != nil || !fi.IsDir() {
		return fmt.Errorf("agent/automations not found — every project ships one (`agentctl init`)")
	}

	// Same mount shape as the contract harness (tests/contract_test.py
	// CANARY_MOUNTS). build_jobs reads trigger-script files at parse
	// time, so the scripts sibling must ride along when shipped.
	argv := []string{
		"run", "--rm",
		"--security-opt", "label=disable",
		"-v", filepath.Join(root, "agent", "spec.json") + ":/opt/agent/spec.json:ro",
		"-v", automations + ":/opt/agent/automations:ro",
	}
	if scripts := filepath.Join(root, "agent", "scripts"); dirExists(scripts) {
		argv = append(argv, "-v", scripts+":/opt/agent/scripts:ro")
	}
	argv = append(argv, "--env-file", "agent/.env.example")
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

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
