//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tankdonut/agent-base/internal/e2e"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/scaffold"
)

// composeChildEnv is the child environment for engine calls: the
// parent's plus PODMAN_COMPOSE_PROVIDER when podman-compose resolves
// on PATH but podman's own provider lookup misses it (it pins
// /usr/bin/podman-compose, which distros move).
func composeChildEnv(extra []string) []string {
	env := os.Environ()
	env = append(env, extra...)
	if _, ok := os.LookupEnv("PODMAN_COMPOSE_PROVIDER"); !ok {
		if path, err := exec.LookPath("podman-compose"); err == nil {
			env = append(env, "PODMAN_COMPOSE_PROVIDER="+path)
		}
	}
	return env
}

// composeEngine resolves the compose invocation once, probing in the
// mandated order: `podman compose`, `docker compose`, then the
// standalone `*-compose` binaries. The probe is a real exec with a
// real exit code — availability by PATH lookup has lied repeatedly.
// The engine name doubles as the argv head for every compose call.
var (
	composeOnce        sync.Once
	composeName        string // "podman" | "docker" | "podman-compose" | "docker-compose"
	composeStandalone  bool   // standalone binaries take no `compose` token
	composeProbeFailed error
)

func composeEngine(t *testing.T) string {
	t.Helper()
	name, err := resolveComposeEngine()
	if err != nil {
		t.Skipf("%v", err)
	}
	return name
}

func resolveComposeEngine() (string, error) {
	composeOnce.Do(func() {
		if pref := os.Getenv("AGENT_E2E_ENGINE"); pref != "" {
			if _, err := exec.LookPath(pref); err == nil {
				composeName = pref
			}
		}
		if composeName == "" {
			candidates := []struct {
				name       string
				standalone bool
			}{
				{"podman", false},
				{"docker", false},
				{"podman-compose", true},
				{"docker-compose", true},
			}
			for _, c := range candidates {
				if _, err := exec.LookPath(c.name); err != nil {
					continue
				}
				argv := []string{c.name}
				if !c.standalone {
					argv = append(argv, "compose")
				}
				probe := exec.Command(argv[0], append(argv[1:], "version")...)
				probe.Stdout = nil
				probe.Stderr = nil
				if err := probe.Run(); err == nil {
					composeName = c.name
					composeStandalone = c.standalone
					break
				}
			}
		}
		if composeName == "" {
			composeProbeFailed = fmt.Errorf("no working compose command — probed `podman compose`, `docker compose`, `podman-compose`, `docker-compose`")
		}
	})
	return composeName, composeProbeFailed
}

// composeArgs renders the engine argv for a compose verb: the
// dispatchers carry a `compose` token, the standalone binaries don't.
func composeArgs(name string, verb ...string) []string {
	argv := []string{name}
	if !isStandaloneCompose(name) {
		argv = append(argv, "compose")
	}
	return append(argv, verb...)
}

func isStandaloneCompose(name string) bool {
	return name == "podman-compose" || name == "docker-compose"
}

// ensureBaseImage resolves the base image for the tier-A fixtures:
// AGENT_E2E_IMAGE when set (CI passes the branch-built candidate, which
// may be digest-pinned), else the pinned public DefaultBaseTag. The
// explicit input is a required identity — unavailable must fail, never
// silently test different bytes; the ambient default stays best-effort
// so a network problem is still a clean skip for fresh checkouts.
func ensureBaseImage(t *testing.T, engine string) string {
	t.Helper()
	override := os.Getenv("AGENT_E2E_IMAGE")
	ref := override
	if ref == "" {
		ref = "ghcr.io/tankdonut/agent-base:" + scaffold.DefaultBaseTag
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := engineInspect(ctx, engine, ref); err == nil {
		return ref
	}
	err := e2e.Run(ctx, e2e.Step{
		Name: "pull-base", Argv: []string{engine, "pull", ref}, Budget: 5 * time.Minute,
	})
	if err != nil {
		if override != "" {
			t.Fatalf("required base image %s unavailable: %v", ref, err)
		}
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
// pinned to the public sentinel, env pair) and a fleet manifest
// allocating ports from `base`. The agent's deployed bytes are pinned
// by overrides.image (imageRef — the ensureBaseImage result), keeping
// the Dockerfile a valid public pin for Derive while compose deploys
// exactly the candidate. Returns the manifest and the agent's dir.
func fixtureAgent(t *testing.T, engine, name string, portBase int, imageRef string) (*fleet.Manifest, string, string) {
	t.Helper()
	root := t.TempDir()
	manifest := fmt.Sprintf(
		"defaults:\n  compose:\n    engine: %s\nagents:\n  %s:\n    gateway_port: %d\n    overrides:\n      image: %s\n",
		engine, name, portBase, imageRef)
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

// writeDockerfile writes the fixture Dockerfile: the five COPY lines
// over a public sentinel FROM — never built (the manifest's
// overrides.image owns the deployed ref), but Derive validates it, so
// it must stay a contract-valid public pin.
func writeDockerfile(t *testing.T, path, ref string) {
	t.Helper()
	if ref == "" {
		ref = "ghcr.io/tankdonut/agent-base:" + scaffold.DefaultBaseTag
	}
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

func standaloneFixup(name string, args []string) (string, []string) {
	if composeStandalone && isStandaloneCompose(name) && len(args) > 0 && args[0] == "compose" {
		return name, args[1:]
	}
	return name, args
}

func (processRunner) Run(env []string, name string, args ...string) error {
	name, args = standaloneFixup(name, args)
	cmd := exec.Command(name, args...)
	cmd.Env = composeChildEnv(env)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (processRunner) RunOutput(env []string, name string, args ...string) ([]byte, error) {
	name, args = standaloneFixup(name, args)
	cmd := exec.Command(name, args...)
	cmd.Env = composeChildEnv(env)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func (processRunner) RunIn(dir string, env []string, name string, args ...string) error {
	name, args = standaloneFixup(name, args)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = composeChildEnv(env)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s (dir=%s): %w", name, strings.Join(args, " "), dir, err)
	}
	return nil
}

func (processRunner) RunOutputIn(dir string, env []string, name string, args ...string) ([]byte, error) {
	name, args = standaloneFixup(name, args)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = composeChildEnv(env)
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
