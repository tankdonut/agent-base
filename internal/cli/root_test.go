package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestCommandTree(t *testing.T) {
	root := NewRootCommand()
	want := map[string]bool{
		"init": false, "version": false, "up": false, "dev": false,
		"down": false, "logs": false, "build-images": false,
		"restart": false, "rebuild": false, "update": false,
		"validate": false, "secrets": false, "env": false,
		"worktree": false, "open": false, "check": false, "hooks": false,
	}
	for _, cmd := range root.Commands() {
		delete(want, cmd.Name())
	}
	for name := range want {
		t.Errorf("missing root command %q", name)
	}

	secrets := child(t, root, "secrets")
	for _, name := range []string{"init", "check", "edit"} {
		if child(t, secrets, name) == nil {
			t.Errorf("missing secrets subcommand %q", name)
		}
	}
	worktree := child(t, root, "worktree")
	for _, name := range []string{"create", "remove"} {
		if child(t, worktree, name) == nil {
			t.Errorf("missing worktree subcommand %q", name)
		}
	}
}

func child(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func TestEveryCommandHasShort(t *testing.T) {
	var check func(c *cobra.Command)
	check = func(c *cobra.Command) {
		if strings.TrimSpace(c.Short) == "" {
			t.Errorf("%s has an empty Short", c.Name())
		}
		for _, sub := range c.Commands() {
			check(sub)
		}
	}
	check(NewRootCommand())
}

func TestVersionOutput(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "agentctl "+Version+"\n" {
		t.Errorf("version output = %q", got)
	}
}

func TestVersionFlagMatches(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "agentctl "+Version+"\n" {
		t.Errorf("--version output = %q", got)
	}
}

func TestUnknownCommandSuggests(t *testing.T) {
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	var errOut bytes.Buffer
	root.SetErr(&errOut)
	root.SetArgs([]string{"verison"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err = %v, want unknown command", err)
	}
	if !strings.Contains(errOut.String(), "version") {
		t.Errorf("stderr lacks suggestion for `version`:\n%s", errOut.String())
	}
}

func TestExecuteMapsErrorsToOne(t *testing.T) {
	if code := runWithArgs(t, "bogus"); code != 1 {
		t.Errorf("exit code for unknown command = %d, want 1", code)
	}
	if code := runWithArgs(t, "version"); code != 0 {
		t.Errorf("exit code for version = %d, want 0", code)
	}
}

// runWithArgs executes the command tree with args, capturing stdio, and
// returns the exit code Execute would produce.
func runWithArgs(t *testing.T, args ...string) int {
	t.Helper()
	root := NewRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		return 1
	}
	return 0
}

func TestSetupViperLayering(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".agentctl.yaml"), []byte("engine: docker\ngateway_port: 19000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()

	viper.Reset()
	if err := setupViper(); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString("engine"); got != "docker" {
		t.Errorf("engine = %q, want docker from .agentctl.yaml", got)
	}
	if got := viper.GetInt("gateway_port"); got != 19000 {
		t.Errorf("gateway_port = %d, want 19000", got)
	}

	t.Setenv("AGENTCTL_ENGINE", "podman")
	viper.Reset()
	if err := setupViper(); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString("engine"); got != "podman" {
		t.Errorf("engine = %q, want podman from AGENTCTL_ENGINE", got)
	}
}

func TestSetupViperReadsProjectRootConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent", "spec.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agentctl.yaml"), []byte("engine: podman\ngateway_port: 18790\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "agent", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, nested)
	defer restore()

	viper.Reset()
	if err := setupViper(); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString("engine"); got != "podman" {
		t.Errorf("engine = %q, want podman from project-root .agentctl.yaml", got)
	}
	if got := viper.GetInt("gateway_port"); got != 18790 {
		t.Errorf("gateway_port = %d, want 18790 from project-root .agentctl.yaml", got)
	}
}

func TestSetupViperCwdConfigWins(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent", "spec.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agentctl.yaml"), []byte("engine: podman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".agentctl.yaml"), []byte("engine: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, nested)
	defer restore()

	viper.Reset()
	if err := setupViper(); err != nil {
		t.Fatal(err)
	}
	if got := viper.GetString("engine"); got != "docker" {
		t.Errorf("engine = %q, want docker — cwd config must win over project root", got)
	}
}

func TestSecretsInitAndEnvAlias(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	example := "#OPENCLAW_GATEWAY_TOKEN=\n#ZAI_API_KEY=\n"
	if err := os.WriteFile(filepath.Join(dir, "agent", ".env.example"), []byte(example), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent", "spec.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, dir)
	defer restore()

	for _, args := range [][]string{{"secrets", "init"}, {"env"}} {
		root := NewRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "agent", ".env")); err != nil {
			t.Fatalf("%v: agent/.env not created", args)
		}
		if err := os.Remove(filepath.Join(dir, "agent", ".env")); err != nil {
			t.Fatal(err)
		}
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Chdir(old); err != nil {
			t.Fatal(err)
		}
	}
}
