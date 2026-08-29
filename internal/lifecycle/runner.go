// Package lifecycle implements agentctl's project commands: compose
// lifecycle, validation, secrets, worktrees, and gateway access. It is
// cobra-free; commands in internal/cli parse flags and inject a Runner
// so tests never need docker or git.
package lifecycle

import "errors"

// errNilRunner surfaces wiring mistakes as errors instead of panics.
var errNilRunner = errors.New("internal: nil runner")

// Runner abstracts process execution. Run starts name with args, wiring
// stdin/stdout/stderr through; env is the full environment (nil inherits
// the parent's). LookPath mirrors exec.LookPath.
type Runner interface {
	Run(env []string, name string, args ...string) error
	LookPath(name string) (string, error)
}

// runArgv executes argv through r, guarding against a nil Runner.
func runArgv(r Runner, env []string, argv ...string) error {
	if r == nil {
		return errNilRunner
	}
	if len(argv) == 0 {
		return errors.New("internal: empty argv")
	}
	return r.Run(env, argv[0], argv[1:]...)
}

// lookPath resolves name through r with the same nil guard.
func lookPath(r Runner, name string) (string, error) {
	if r == nil {
		return "", errors.New("internal: nil runner")
	}
	return r.LookPath(name)
}
