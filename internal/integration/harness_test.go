//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tankdonut/agent-base/internal/e2e"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/scaffold"
)

// pickEngine prefers the explicit AGENT_E2E_ENGINE, then podman (the
// local-first convention), then docker. No engine — the tier has
// nothing to say.
func pickEngine(t *testing.T) string {
	t.Helper()
	if pref := os.Getenv("AGENT_E2E_ENGINE"); pref != "" {
		if _, err := exec.LookPath(pref); err != nil {
			t.Skipf("AGENT_E2E_ENGINE=%s not found", pref)
		}
		return pref
	}
	for _, candidate := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("no container engine (podman/docker) on PATH — Tier A needs a real engine")
	return ""
}

// ensureBaseImage pulls the pinned base image once per run; compose
// build would pull lazily anyway, but an explicit contained pull turns
// a network problem into a clean skip instead of a build failure.
func ensureBaseImage(t *testing.T, engine string) string {
	t.Helper()
	tag := scaffold.DefaultBaseTag
	ref := "ghcr.io/tankdonut/agent-base:" + tag
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := engineInspect(ctx, engine, ref); err == nil {
		return ref
	}
	err := e2e.Run(ctx, e2e.Step{
		Name: "pull-base", Argv: []string{engine, "pull", ref}, Budget: 5 * time.Minute,
	})
	if err != nil {
		t.Skipf("base image %s unavailable: %v", ref, err)
	}
	return ref
}

func engineInspect(ctx context.Context, engine, ref string) error {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx2, engine, "inspect", ref).Run()
}

// fixtureAgent writes one contract-complete agent (spec, Dockerfile
// pinned to the base, env pair) and a fleet manifest allocating ports
// from `base`. Returns the manifest and the agent's dir.
func fixtureAgent(t *testing.T, engine, name string, portBase int) (*fleet.Manifest, string, string) {
	t.Helper()
	root := t.TempDir()
	manifest := fmt.Sprintf("defaults:\n  compose:\n    engine: %s\nagents:\n  %s:\n    gateway_port: %d\n", engine, name, portBase)
	if err := os.WriteFile(filepath.Join(root, "fleet.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"spec.json":    `{"agent":{"name":"` + name + `"}}`,
		".env.example": "#ZAI_API_KEY=\n",
		".env":         "ZAI_API_KEY=integration-canary\n",
		// The litellm sidecar ships in the default (plane-less)
		// envelope; compose engines validate env_file existence at
		// up-time, so the mirrored pair must exist on disk.
		"litellm/.env.example": "#LITELLM_MASTER_KEY=\n#LITELLM_API_KEY=\n",
		"litellm/.env":         "LITELLM_MASTER_KEY=sk-integration-master\nLITELLM_API_KEY=sk-integration-agent\n",
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := fleet.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir, filepath.Join(dir, "Dockerfile")
}

// writeDockerfile pins the fixture's FROM at the pulled base tag.
func writeDockerfile(t *testing.T, path, ref string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("FROM "+ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// freePort asks the kernel for an unused loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// newRunner is the real exec runner — integration means real.
func newRunner() process.Runner { return processRunner{} }

type processRunner struct{}

func (processRunner) Run(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (processRunner) RunOutput(env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func (processRunner) RunIn(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (processRunner) RunOutputIn(dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func (processRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// fileOutput adapts a file to platform.Output.
type fileOutput struct{ f *os.File }

func (o fileOutput) Printf(format string, args ...any) {
	fmt.Fprintf(o.f, format, args...)
}
