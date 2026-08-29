package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/templates"
)

// runRoot executes the command tree with args and captured stdio.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestInitCmdScaffoldsGoldenTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-agent")
	out, err := runRoot(t, "init", dir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	want, err := templates.Paths()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	var found []string
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("scaffolded tree mismatch:\n got  %v\n want %v", found, want)
	}

	spec, err := os.ReadFile(filepath.Join(dir, "agent", "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(spec) {
		t.Errorf("rendered spec.json is not valid JSON")
	}
	if fi, err := os.Stat(filepath.Join(dir, "make.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("make.sh mode = %v, want 0755 (%v)", fi, err)
	}
	if !strings.Contains(out, "agentctl secrets init") {
		t.Errorf("init output lacks next-steps pointer:\n%s", out)
	}
}

func TestInitCmdAppliesFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "flagged")
	_, err := runRoot(t, "init", dir,
		"--gateway-port", "8080",
		"--telegram=false",
		"--agent-name", "Flag Bot",
	)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	compose, err := os.ReadFile(filepath.Join(dir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "8080") {
		t.Errorf("compose.yml does not carry --gateway-port 8080")
	}
	spec, err := os.ReadFile(filepath.Join(dir, "agent", "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(spec), "telegram") {
		t.Errorf("spec.json wires telegram despite --telegram=false")
	}
	if !strings.Contains(string(spec), "Flag Bot") {
		t.Errorf("spec.json lacks the --agent-name value")
	}
}

func TestInitCmdErrorPaths(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "non-empty target without --force",
			args: []string{"init", func() string {
				d := t.TempDir()
				os.WriteFile(filepath.Join(d, "keep.txt"), []byte("x"), 0o644)
				return d
			}()},
			want: "--force",
		},
		{name: "bad base tag", args: []string{"init", filepath.Join(t.TempDir(), "x"), "--base-tag", "latest"}, want: "--base-tag"},
		{name: "zero args", args: []string{"init"}, want: "accepts 1 arg(s), received 0"},
		{name: "two args", args: []string{"init", "a", "b"}, want: "accepts 1 arg(s), received 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runRoot(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want error containing %q", err, tt.want)
			}
		})
	}
}
