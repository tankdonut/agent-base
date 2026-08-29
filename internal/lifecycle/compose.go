package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
)

// composeArgv builds the base compose invocation for the project root:
// <engine> compose -f compose.yml [-f compose.dev.yml] <verb...>. Paths
// are relative, so callers must run with the project root as cwd.
func composeArgv(engine string, dev bool, verb ...string) []string {
	argv := []string{engine, "compose", "-f", "compose.yml"}
	if dev {
		argv = append(argv, "-f", "compose.dev.yml")
	}
	return append(argv, verb...)
}

// requireEnvFile is the secrets gate for start commands: compose mounts
// agent/.env via env_file, so a missing file fails deep inside the
// engine. Fail early with the fix instead.
func requireEnvFile(root string) error {
	if _, err := os.Stat(filepath.Join(root, "agent", ".env")); err != nil {
		return fmt.Errorf("agent/.env not found — run `agentctl secrets init` first")
	}
	return nil
}

// Up gates on agent/.env, then starts the production stack detached.
func Up(r Runner, engine, root string) error {
	if r == nil {
		return errNilRunner
	}
	if err := requireEnvFile(root); err != nil {
		return err
	}
	return runArgv(r, nil, composeArgv(engine, false, "up", "-d")...)
}

// Dev gates on agent/.env, then starts the stack with the hot-reload
// overlay (compose.dev.yml) applied on top of compose.yml.
func Dev(r Runner, engine, root string) error {
	if r == nil {
		return errNilRunner
	}
	if err := requireEnvFile(root); err != nil {
		return err
	}
	return runArgv(r, nil, composeArgv(engine, true, "up", "-d")...)
}

// Down stops the stack.
func Down(r Runner, engine string) error {
	if r == nil {
		return errNilRunner
	}
	return runArgv(r, nil, composeArgv(engine, false, "down")...)
}

// Logs shows compose logs; args pass through untouched (e.g. -f agent).
func Logs(r Runner, engine string, args []string) error {
	if r == nil {
		return errNilRunner
	}
	verb := append([]string{"logs"}, args...)
	return runArgv(r, nil, composeArgv(engine, false, verb...)...)
}

// BuildImages builds the project image(s).
func BuildImages(r Runner, engine string) error {
	if r == nil {
		return errNilRunner
	}
	return runArgv(r, nil, composeArgv(engine, false, "build")...)
}

// Restart restarts the named services (all when none given).
func Restart(r Runner, engine string, services []string) error {
	if r == nil {
		return errNilRunner
	}
	verb := append([]string{"restart"}, services...)
	return runArgv(r, nil, composeArgv(engine, false, verb...)...)
}

// Rebuild rebuilds the named services' images and force-recreates them
// (all services when none given).
func Rebuild(r Runner, engine string, services []string) error {
	if r == nil {
		return errNilRunner
	}
	build := append([]string{"build"}, services...)
	if err := runArgv(r, nil, composeArgv(engine, false, build...)...); err != nil {
		return err
	}
	recreate := append([]string{"up", "-d", "--force-recreate"}, services...)
	return runArgv(r, nil, composeArgv(engine, false, recreate...)...)
}

// Update fast-forwards the project repo, then rebuilds and recreates
// the whole stack.
func Update(r Runner, engine, root string) error {
	if r == nil {
		return errNilRunner
	}
	if err := runArgv(r, nil, "git", "pull", "--ff-only"); err != nil {
		return fmt.Errorf("git pull: %w", err)
	}
	return Rebuild(r, engine, nil)
}
