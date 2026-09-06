// Package process is agentctl's process-execution port: the Runner
// abstraction every shell-out flows through (compose, git, editors,
// xdg-open, platform CLIs), plus the engine-name policy. It is
// cobra-free and depends on nothing internal — a foundation package.
package process

import "errors"

// ErrNilRunner surfaces wiring mistakes as errors instead of panics.
var ErrNilRunner = errors.New("internal: nil runner")

// Runner abstracts process execution. Run starts name with args, wiring
// stdin/stdout/stderr through; env is the full environment (nil inherits
// the parent's). LookPath mirrors exec.LookPath.
type Runner interface {
	Run(env []string, name string, args ...string) error
	LookPath(name string) (string, error)
}

// RunArgv executes argv through r, guarding against a nil Runner.
func RunArgv(r Runner, env []string, argv ...string) error {
	if r == nil {
		return ErrNilRunner
	}
	if len(argv) == 0 {
		return errors.New("internal: empty argv")
	}
	return r.Run(env, argv[0], argv[1:]...)
}

// LookPath resolves name through r with the same nil guard.
func LookPath(r Runner, name string) (string, error) {
	if r == nil {
		return "", ErrNilRunner
	}
	return r.LookPath(name)
}
