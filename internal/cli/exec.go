package cli

import (
	"os"
	"os/exec"

	"github.com/tankdonut/agent-base/internal/process"
)

// execRunner is the real Runner: exec with inherited stdio. A nil env
// inherits the parent environment (exec semantics).
type execRunner struct{}

func (execRunner) Run(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (execRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// newRunner builds the Runner injected into lifecycle and platform
// calls. A var so tests can stub process execution hermetically.
var newRunner = func() process.Runner { return execRunner{} }
