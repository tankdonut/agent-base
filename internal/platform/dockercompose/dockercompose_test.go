package dockercompose

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
)

type fakeRunner struct {
	calls [][]string
	look  map[string]bool
}

func newFakeRunner(look ...string) *fakeRunner {
	r := &fakeRunner{look: map[string]bool{}}
	for _, n := range look {
		r.look[n] = true
	}
	return r
}

func (f *fakeRunner) Run(env []string, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func (f *fakeRunner) RunOutput(env []string, name string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("fake: output capture not configured")
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.look[name] {
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("%s: not found", name)
}

type recorder struct{ bytes.Buffer }

func (r *recorder) Printf(format string, a ...any) { fmt.Fprintf(r, format, a...) }

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const fixtureCompose = `name: fixture-agent
services:
  agent:
    build: {context: ., dockerfile: agent/Dockerfile}
volumes:
  agent-data:
  agent-backups:
`

const fixtureSpec = `{"specVersion": 1}`

func fixtureProject(t *testing.T) string {
	return writeProject(t, map[string]string{
		"agent/spec.json":  fixtureSpec,
		"agent/Dockerfile": "FROM ghcr.io/tankdonut/agent-base:2026.09.05\n",
		"agent/.env":       "ZAI_API_KEY=x\n",
		"compose.yml":      fixtureCompose,
	})
}

func newAdapter(t *testing.T, r process.Runner, ns map[string]any) platform.Platform {
	t.Helper()
	p, err := New(r, ns)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func deployment(t *testing.T, root string) platform.Deployment {
	t.Helper()
	d, err := platform.Derive(root)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func assertCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestNewResolvesEngineAndConfig(t *testing.T) {
	r := newFakeRunner("podman", "docker")
	p, err := New(r, map[string]any{"engine": "docker", "gateway_port": 19000})
	if err != nil {
		t.Fatal(err)
	}
	a := p.(*Adapter)
	if a.engine != "docker" {
		t.Errorf("engine = %q, want docker from config", a.engine)
	}
	if a.gatewayPort != 19000 {
		t.Errorf("gatewayPort = %d, want 19000", a.gatewayPort)
	}
	if a.Capabilities() != (platform.Capabilities{Exec: true, StopStart: true, VolumePreservingDestroy: true}) {
		t.Errorf("capabilities = %+v", a.Capabilities())
	}
}

func TestNewFailsClosedOnBadConfig(t *testing.T) {
	r := newFakeRunner("podman")
	tests := []struct {
		name string
		ns   map[string]any
		want string
	}{
		{"unknown key", map[string]any{"turbo": true}, "unknown compose config key"},
		{"engine type", map[string]any{"engine": 7}, "want a string"},
		{"port type", map[string]any{"gateway_port": "x"}, "want an integer"},
		{"bad engine name", map[string]any{"engine": "containerd"}, "invalid engine"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(r, tt.ns)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNewWithoutEngineOnHost(t *testing.T) {
	if _, err := New(newFakeRunner(), nil); err == nil || !strings.Contains(err.Error(), "no container engine") {
		t.Fatalf("err = %v, want install hint", err)
	}
}

func TestCheckContract(t *testing.T) {
	r := newFakeRunner("podman")
	p := newAdapter(t, r, nil)

	if err := p.Check(fixtureProject(t), &platform.Deployment{}); err != nil {
		t.Fatalf("contract project: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(root string)
		want   string
	}{
		{"no compose.yml", func(root string) {
			os.Remove(filepath.Join(root, "compose.yml"))
		}, "compose.yml not found"},
		{"no agent service", func(root string) {
			os.WriteFile(filepath.Join(root, "compose.yml"), []byte("name: x\nservices: {}\nvolumes:\n  agent-data:\n  agent-backups:\n"), 0o644)
		}, "`agent` service"},
		{"missing data volume", func(root string) {
			os.WriteFile(filepath.Join(root, "compose.yml"), []byte("name: x\nservices:\n  agent: {}\nvolumes:\n  agent-backups:\n"), 0o644)
		}, "agent-data"},
		{"missing env file", func(root string) {
			os.Remove(filepath.Join(root, "agent", ".env"))
		}, "secrets init"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fixtureProject(t)
			tt.mutate(root)
			err := p.Check(root, &platform.Deployment{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestDeployArgv(t *testing.T) {
	root := fixtureProject(t)
	d := deployment(t, root)
	ctx := context.Background()

	t.Run("plain builds then ups", func(t *testing.T) {
		r := newFakeRunner("podman")
		p := newAdapter(t, r, nil)
		out := &recorder{}
		if err := p.Deploy(ctx, r, root, &d, platform.DeployOptions{}, out); err != nil {
			t.Fatal(err)
		}
		assertCalls(t, r.calls, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build"},
			{"podman", "compose", "-f", "compose.yml", "up", "-d"},
		})
	})

	t.Run("force rebuilds", func(t *testing.T) {
		r := newFakeRunner("podman")
		p := newAdapter(t, r, nil)
		if err := p.Deploy(ctx, r, root, &d, platform.DeployOptions{Force: true}, &recorder{}); err != nil {
			t.Fatal(err)
		}
		assertCalls(t, r.calls, [][]string{
			{"podman", "compose", "-f", "compose.yml", "build"},
			{"podman", "compose", "-f", "compose.yml", "up", "-d", "--force-recreate"},
		})
	})

	t.Run("dry run checks only", func(t *testing.T) {
		r := newFakeRunner("podman")
		p := newAdapter(t, r, nil)
		out := &recorder{}
		if err := p.Deploy(ctx, r, root, &d, platform.DeployOptions{DryRun: true}, out); err != nil {
			t.Fatal(err)
		}
		if len(r.calls) != 0 {
			t.Errorf("dry run must not exec, got %v", r.calls)
		}
		if !strings.Contains(out.String(), "dry run complete") {
			t.Errorf("out = %q", out.String())
		}
	})
}

func TestVerbArgv(t *testing.T) {
	root := fixtureProject(t)
	d := deployment(t, root)
	ctx := context.Background()
	r := newFakeRunner("podman")
	p := newAdapter(t, r, nil)

	if err := p.Status(ctx, r, root, &d, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-1:], [][]string{{"podman", "compose", "-f", "compose.yml", "ps"}})
	if err := p.Logs(ctx, r, root, &d, true, &recorder{}); err != nil {
		t.Fatal(err)
	}
	last := r.calls[len(r.calls)-1]
	if strings.Join(last, " ") != "podman compose -f compose.yml logs -f agent" {
		t.Errorf("logs argv = %v", last)
	}

	if err := p.Mcp(ctx, r, root, &d, []string{"login"}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	last = r.calls[len(r.calls)-1]
	if strings.Join(last, " ") != "podman compose -f compose.yml exec agent openclaw mcp login" {
		t.Errorf("mcp argv = %v", last)
	}

	if err := p.Stop(ctx, r, root, &d, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx, r, root, &d, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(ctx, r, root, &d, true, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-3:], [][]string{
		{"podman", "compose", "-f", "compose.yml", "stop"},
		{"podman", "compose", "-f", "compose.yml", "start"},
		{"podman", "compose", "-f", "compose.yml", "down", "-v"},
	})
}
