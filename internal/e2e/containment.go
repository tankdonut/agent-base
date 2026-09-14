// Package e2e is the containment harness for engine-touching tests:
// every engine child runs in its own process group under a hard
// budget, and a breach costs the whole group (SIGKILL) plus an
// artifact dump — compose state, engine logs, the child's own stderr.
// A wedge therefore fails loudly with evidence instead of hanging the
// suite. Imports nothing internal (foundation package).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultArtifactRoot is where dumps land when Step.ArtifactRoot is
// empty — the repo's existing e2e log convention.
const DefaultArtifactRoot = "logs/e2e"

// tailLines is how much of the child's output an error carries; the
// full stream always lands in the artifact dir.
const tailLines = 40

// Step is one contained engine command.
type Step struct {
	// Name prefixes the artifact directory (e.g. "plane-up").
	Name string
	// Dir is the child's working directory.
	Dir string
	// Env is the full environment (nil inherits).
	Env []string
	// Argv is the command to run.
	Argv []string
	// Budget bounds the child's runtime; breach = group SIGKILL.
	// Zero means 240s.
	Budget time.Duration
	// ArtifactRoot overrides DefaultArtifactRoot (tests use t.TempDir).
	ArtifactRoot string
	// Dumpers run inside the artifact dir after a failure — compose
	// ps, engine logs, anything that explains the corpse.
	Dumpers []func(dir string) error
	// OnStart exposes the child's process group id (tests assert the
	// group actually died).
	OnStart func(pgid int)
}

// Result reports how a step ended.
type Result struct {
	ArtifactDir string
	TimedOut    bool
}

// Run executes the step under containment. A budget breach or
// non-zero exit returns an error carrying the artifact dir and the
// output tail; ctx cancellation kills the group the same way.
func Run(ctx context.Context, s Step) error {
	if len(s.Argv) == 0 {
		return fmt.Errorf("containment: empty argv")
	}
	if s.Budget <= 0 {
		s.Budget = 240 * time.Second
	}
	root := s.ArtifactRoot
	if root == "" {
		root = DefaultArtifactRoot
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	dir := filepath.Join(root, fmt.Sprintf("%s-%s", s.Name, stamp))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("containment: artifact dir: %w", err)
	}

	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	outFile, err := os.Create(filepath.Join(dir, "stderr.log"))
	if err != nil {
		return fmt.Errorf("containment: stderr capture: %w", err)
	}
	defer outFile.Close()

	var mu sync.Mutex
	var lines []string
	cmd.Stdout = writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		all := strings.Split(strings.TrimRight(string(p), "\n"), "\n")
		lines = append(lines, all...)
		if len(lines) > tailLines {
			lines = lines[len(lines)-tailLines:]
		}
		mu.Unlock()
		return outFile.Write(p)
	})
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("containment: start: %w (artifacts: %s)", err, dir)
	}
	pgid := cmd.Process.Pid // Setpgid makes the group id == child pid
	if s.OnStart != nil {
		s.OnStart(pgid)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	breach := false
	timer := time.AfterFunc(s.Budget, func() {
		breach = true
		killGroup(pgid)
	})
	select {
	case err := <-done:
		timer.Stop()
		// A racing timer may have fired just as the child exited.
		_ = breach
		if err == nil {
			return nil
		}
		s.dump(dir)
		return fmt.Errorf("containment: %s exited: %v (artifacts: %s)\n%s",
			s.Argv[0], err, dir, strings.Join(tail(&mu, lines), "\n"))
	case <-ctx.Done():
		timer.Stop()
		killGroup(pgid)
		<-done
		s.dump(dir)
		return fmt.Errorf("containment: %s cancelled (artifacts: %s)\n%s",
			s.Argv[0], dir, strings.Join(tail(&mu, lines), "\n"))
	case <-time.After(s.Budget + 5*time.Second):
		// The kill timer fired but Wait hasn't returned — a wedged
		// grandchild holding the pipe. Kill again and stop waiting.
		killGroup(pgid)
		s.dump(dir)
		return fmt.Errorf("containment: %s breached its %s budget and ignored SIGKILL (artifacts: %s)\n%s",
			s.Argv[0], s.Budget, dir, strings.Join(tail(&mu, lines), "\n"))
	}
}

// killGroup SIGKILLs every process in the group — children and
// grandchildren die with the leader, so orphans cannot outlive the
// suite.
func killGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// dump runs the step's dumpers, absorbing their errors — a failing
// dumper must not mask the original failure.
func (s Step) dump(dir string) {
	for i, d := range s.Dumpers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("dumper-%d-panic.txt", i)),
						[]byte(fmt.Sprint(r)), 0o644)
				}
			}()
			_ = d(dir)
		}()
	}
}

func tail(mu *sync.Mutex, lines []string) []string {
	mu.Lock()
	defer mu.Unlock()
	if len(lines) > tailLines {
		return lines[len(lines)-tailLines:]
	}
	return lines
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
