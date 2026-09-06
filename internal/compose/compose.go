package compose

import (
	"fmt"
	"github.com/tankdonut/agent-base/internal/process"
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

// RequireEnvFile is the secrets gate for start commands: compose mounts
// agent/.env via env_file, so a missing file fails deep inside the
// engine. Fail early with the fix instead. Exported for platform
// adapters that gate their own deploy paths.
func RequireEnvFile(root string) error {
	if _, err := os.Stat(filepath.Join(root, "agent", ".env")); err != nil {
		return fmt.Errorf("agent/.env not found — run `agentctl secrets init` first")
	}
	return nil
}

// Up gates on agent/.env, then starts the production stack detached.
func Up(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if err := RequireEnvFile(root); err != nil {
		return err
	}
	return process.RunArgv(r, nil, composeArgv(engine, false, "up", "-d")...)
}

// Dev gates on agent/.env, then starts the stack with the hot-reload
// overlay (compose.dev.yml) applied on top of compose.yml.
func Dev(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if err := RequireEnvFile(root); err != nil {
		return err
	}
	return process.RunArgv(r, nil, composeArgv(engine, true, "up", "-d")...)
}

// Down removes the stack's containers and networks; named volumes
// (agent-data, agent-backups) survive.
func Down(r process.Runner, engine string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgv(r, nil, composeArgv(engine, false, "down")...)
}

// Stop pauses the running containers in place (no removal); Start
// resumes them. The pair exists for capability-gated platform verbs.
func Stop(r process.Runner, engine string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgv(r, nil, composeArgv(engine, false, "stop")...)
}

// Start resumes containers stopped with Stop.
func Start(r process.Runner, engine string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgv(r, nil, composeArgv(engine, false, "start")...)
}

// Ps lists the stack's containers and their state.
func Ps(r process.Runner, engine string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgv(r, nil, composeArgv(engine, false, "ps")...)
}

// Destroy removes the stack. volumes=false keeps the named volumes
// (warm {data} survives); volumes=true also deletes them — the
// explicit data-loss path.
func Destroy(r process.Runner, engine string, volumes bool) error {
	if r == nil {
		return process.ErrNilRunner
	}
	verb := []string{"down"}
	if volumes {
		verb = append(verb, "-v")
	}
	return process.RunArgv(r, nil, composeArgv(engine, false, verb...)...)
}

// Logs shows compose logs; args pass through untouched (e.g. -f agent).
func Logs(r process.Runner, engine string, args []string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	verb := append([]string{"logs"}, args...)
	return process.RunArgv(r, nil, composeArgv(engine, false, verb...)...)
}

// Mcp passes args to `openclaw mcp` inside the running agent container
// (login/logout/status/doctor/…). Stdio is inherited, so interactive
// flows — the OAuth login URL print and the --code paste-back — work
// verbatim against the pinned CLI.
func Mcp(r process.Runner, engine string, args []string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	verb := append([]string{"exec", "agent", "openclaw", "mcp"}, args...)
	return process.RunArgv(r, nil, composeArgv(engine, false, verb...)...)
}

// BuildImages builds the project image(s).
func BuildImages(r process.Runner, engine string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgv(r, nil, composeArgv(engine, false, "build")...)
}

// Restart restarts the named services (all when none given).
func Restart(r process.Runner, engine string, services []string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	verb := append([]string{"restart"}, services...)
	return process.RunArgv(r, nil, composeArgv(engine, false, verb...)...)
}

// Rebuild rebuilds the named services' images and force-recreates them
// (all services when none given).
func Rebuild(r process.Runner, engine string, services []string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	build := append([]string{"build"}, services...)
	if err := process.RunArgv(r, nil, composeArgv(engine, false, build...)...); err != nil {
		return err
	}
	recreate := append([]string{"up", "-d", "--force-recreate"}, services...)
	return process.RunArgv(r, nil, composeArgv(engine, false, recreate...)...)
}
