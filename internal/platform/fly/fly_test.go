package fly

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
	calls    [][]string
	look     map[string]bool
	outputs  map[string]string
	failArgv [][]string
}

func newFakeRunner(look ...string) *fakeRunner {
	r := &fakeRunner{look: map[string]bool{}, outputs: map[string]string{}}
	for _, n := range look {
		r.look[n] = true
	}
	return r
}

func (f *fakeRunner) Run(env []string, name string, args ...string) error {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	for _, bad := range f.failArgv {
		if strings.Join(call, " ") == strings.Join(bad, " ") {
			return fmt.Errorf("fake failure: %v", call)
		}
	}
	return nil
}

func (f *fakeRunner) RunOutput(env []string, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	if out, ok := f.outputs[key]; ok {
		return []byte(out), nil
	}
	return []byte("[]"), nil
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

const fixtureSpec = `{
  "specVersion": 1,
  "setup": {"auth_choice": "zai-coding-global"},
  "model": {"fallback": "{env:FALLBACK_MODEL}"}
}`

func fixtureProject(t *testing.T) string {
	root := writeProject(t, map[string]string{
		"agent/spec.json":  fixtureSpec,
		"agent/Dockerfile": "FROM ghcr.io/tankdonut/agent-base:2026.09.05\n",
		"agent/.env":       "FALLBACK_MODEL=m\nZAI_API_KEY=k\n",
	})
	if err := ScaffoldConfig(root, "my-agent", "sjc"); err != nil {
		t.Fatal(err)
	}
	return root
}

func deployment(t *testing.T, root string) platform.Deployment {
	t.Helper()
	d, err := platform.Derive(root)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newAdapter(t *testing.T, r process.Runner, ns map[string]any) platform.Platform {
	t.Helper()
	p, err := New(r, ns)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func assertCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestNewFailsClosed(t *testing.T) {
	r := newFakeRunner("fly")
	if _, err := New(r, map[string]any{"turbo": true}); err == nil || !strings.Contains(err.Error(), "unknown fly config key") {
		t.Fatalf("err = %v, want unknown key", err)
	}
	if _, err := New(r, map[string]any{"app": 7}); err == nil || !strings.Contains(err.Error(), "want a string") {
		t.Fatalf("err = %v, want type error", err)
	}
	if _, err := New(newFakeRunner(), nil); err == nil || !strings.Contains(err.Error(), "fly CLI not found") {
		t.Fatalf("err = %v, want flyctl install hint", err)
	}
}

func TestScaffoldConfig(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldConfig(root, "", "sjc"); err == nil || !strings.Contains(err.Error(), "--app") {
		t.Fatalf("err = %v, want --app requirement", err)
	}
	if err := ScaffoldConfig(root, "my-agent", ""); err != nil {
		t.Fatalf("region must default: %v", err)
	}
	if err := ScaffoldConfig(root, "other", "iad"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want overwrite refusal", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `app = "my-agent"`) {
		t.Errorf("rendered config = %s", data)
	}
	if !strings.Contains(string(data), `primary_region = "iad"`) {
		t.Errorf("region default not rendered: %s", data)
	}
}

func TestCheckContract(t *testing.T) {
	r := newFakeRunner("fly")
	p := newAdapter(t, r, nil)

	if err := p.Check(fixtureProject(t), &platform.Deployment{}); err != nil {
		t.Fatalf("scaffolded manifest must pass: %v", err)
	}

	mutations := []struct {
		name  string
		find  string
		autop string
		want  string
	}{
		{"wrong kill signal", `kill_signal = "SIGTERM"`, `kill_signal = "SIGINT"`, "SIGTERM"},
		{"timeout over cap", "kill_timeout = 300", "kill_timeout = 600", "hard-caps"},
		{"grace over timeout", `AGENT_SHUTDOWN_GRACE = "300"`, `AGENT_SHUTDOWN_GRACE = "600"`, "between 1 and kill_timeout"},
		{"wrong dockerfile", `dockerfile = "agent/Dockerfile"`, `dockerfile = "Dockerfile"`, "agent/Dockerfile"},
		{"wrong mount path", `destination = "/home/node/.openclaw"`, `destination = "/data"`, "/home/node/.openclaw"},
		{"wrong gateway port", "internal_port = 18789", "internal_port = 8080", "gateway port"},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			root := fixtureProject(t)
			path := filepath.Join(root, ConfigName)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), tt.find) {
				t.Fatalf("fixture lacks %q — mutation setup bug", tt.find)
			}
			if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte(tt.find), []byte(tt.autop)), 0o644); err != nil {
				t.Fatal(err)
			}
			err = p.Check(root, &platform.Deployment{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}

	t.Run("no manifest", func(t *testing.T) {
		root := writeProject(t, map[string]string{"agent/spec.json": fixtureSpec})
		err := p.Check(root, &platform.Deployment{})
		if err == nil || !strings.Contains(err.Error(), "platform set fly") {
			t.Fatalf("err = %v, want scaffold hint", err)
		}
	})

	t.Run("incomplete secrets", func(t *testing.T) {
		root := fixtureProject(t)
		if err := os.WriteFile(filepath.Join(root, "agent", ".env"), []byte("FALLBACK_MODEL=m\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := p.Check(root, &platform.Deployment{})
		if err == nil || !strings.Contains(err.Error(), "ZAI_API_KEY") {
			t.Fatalf("err = %v, want missing-secrets error", err)
		}
	})
}

func TestVerbs(t *testing.T) {
	root := fixtureProject(t)
	d := deployment(t, root)
	ctx := context.Background()
	r := newFakeRunner("fly")
	p := newAdapter(t, r, nil)

	if got := p.Capabilities(); got != (platform.Capabilities{Exec: true, StopStart: true, VolumePreservingDestroy: false}) {
		t.Errorf("capabilities = %+v", got)
	}

	if err := p.Deploy(ctx, r, root, &d, platform.DeployOptions{}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-1:], [][]string{
		{"fly", "deploy", "-c", "deploy/fly.toml", "--remote-only"},
	})

	if err := p.Deploy(ctx, r, root, &d, platform.DeployOptions{DryRun: true}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 {
		t.Errorf("dry run must not exec, got %v", r.calls)
	}

	if err := p.Status(ctx, r, root, &d, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-1:], [][]string{{"fly", "status", "-a", "my-agent"}})

	if err := p.Logs(ctx, r, root, &d, true, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-1:], [][]string{{"fly", "logs", "-a", "my-agent"}})

	if err := p.Mcp(ctx, r, root, &d, []string{"login", "docs", "--code", "abc"}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-1:], [][]string{
		{"fly", "ssh", "console", "-a", "my-agent", "-C", "openclaw mcp login docs --code abc"},
	})
}

func TestStopStartDestroyViaMachineList(t *testing.T) {
	root := fixtureProject(t)
	d := deployment(t, root)
	ctx := context.Background()
	r := newFakeRunner("fly")
	// machine list returns one machine.
	r.outputs["fly machine list -a my-agent -j"] = `[{"id":"m-123","state":"started"}]`
	p := newAdapter(t, r, nil)

	if err := p.Stop(ctx, r, root, &d, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx, r, root, &d, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-4:], [][]string{
		{"fly", "machine", "list", "-a", "my-agent", "-j"},
		{"fly", "machine", "stop", "m-123", "-a", "my-agent"},
		{"fly", "machine", "list", "-a", "my-agent", "-j"},
		{"fly", "machine", "start", "m-123", "-a", "my-agent"},
	})

	if err := p.Destroy(ctx, r, root, &d, true, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-1:], [][]string{
		{"fly", "apps", "destroy", "my-agent", "-y"},
	})
}

func TestMachineListEmptyFails(t *testing.T) {
	root := fixtureProject(t)
	d := deployment(t, root)
	r := newFakeRunner("fly")
	r.outputs["fly machine list -a my-agent -j"] = `[]`
	p := newAdapter(t, r, nil)
	err := p.Stop(context.Background(), r, root, &d, &recorder{})
	if err == nil || !strings.Contains(err.Error(), "no machines") {
		t.Fatalf("err = %v, want no-machines error", err)
	}
}

func TestAppConfigOverrideBeatsManifest(t *testing.T) {
	root := fixtureProject(t)
	d := deployment(t, root)
	r := newFakeRunner("fly")
	p := newAdapter(t, r, map[string]any{"app": "renamed-agent"})
	if err := p.Status(context.Background(), r, root, &d, &recorder{}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls[len(r.calls)-1:], [][]string{{"fly", "status", "-a", "renamed-agent"}})
}
