package scaffold

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// goldenPaths is the exact file manifest `init` must produce.
var goldenPaths = []string{
	".agentctl.yaml",
	".github/workflows/ci.yml",
	".gitignore",
	".markdownlint-cli2.yaml",
	".pre-commit-config.yaml",
	"AGENTS.md",
	"README.md",
	"agent/.env.example",
	"agent/Dockerfile",
	"agent/automations/daily-briefing.md",
	"agent/skills/.gitkeep",
	"agent/spec.json",
	"agent/workspace/AGENTS.md",
	"agent/workspace/MEMORY.md",
	"agent/workspace/SOUL.md",
	"agent/workspace/USER.md",
	"compose.dev.yml",
	"compose.yml",
	"knowledge/content/index.md",
	"make.sh",
	"renovate.json",
}

func validConfig(dir string) Config {
	return Config{
		TargetDir:   dir,
		ProjectName: filepath.Base(dir),
		AgentName:   "TestBot",
		BaseTag:     "2026.08.28",
		Model:       "zai/glm-5.2",
		GatewayPort: 18789,
		Telegram:    true,
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
		want string // "" expects success; otherwise an error containing want
	}{
		{name: "valid", mut: func(*Config) {}, want: ""},
		{name: "valid tag with run suffix", mut: func(c *Config) { c.BaseTag = "2026.08.28.3" }, want: ""},
		{name: "valid max port", mut: func(c *Config) { c.GatewayPort = 65535 }, want: ""},
		{name: "tag not a date", mut: func(c *Config) { c.BaseTag = "latest" }, want: "--base-tag"},
		{name: "tag partial date", mut: func(c *Config) { c.BaseTag = "2026.08" }, want: "--base-tag"},
		{name: "tag empty", mut: func(c *Config) { c.BaseTag = "" }, want: "--base-tag"},
		{name: "tag trailing garbage", mut: func(c *Config) { c.BaseTag = "2026.08.28-beta" }, want: "--base-tag"},
		{name: "agent name empty", mut: func(c *Config) { c.AgentName = "" }, want: "--agent-name"},
		{name: "project name empty", mut: func(c *Config) { c.ProjectName = "" }, want: "project name"},
		{name: "target dir empty", mut: func(c *Config) { c.TargetDir = "" }, want: "target directory"},
		{name: "model empty", mut: func(c *Config) { c.Model = "" }, want: "--model"},
		{name: "port zero", mut: func(c *Config) { c.GatewayPort = 0 }, want: "--gateway-port"},
		{name: "port negative", mut: func(c *Config) { c.GatewayPort = -1 }, want: "--gateway-port"},
		{name: "port above 65535", mut: func(c *Config) { c.GatewayPort = 65536 }, want: "--gateway-port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t.TempDir())
			tt.mut(&cfg)
			err := cfg.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestDefaultAgentName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"my-agent", "My Agent"},
		{"grow_agent", "Grow Agent"},
		{"trade-agent", "Trade Agent"},
		{"single", "Single"},
	}
	for _, tt := range tests {
		if got := DefaultAgentName(tt.in); got != tt.want {
			t.Errorf("DefaultAgentName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRunGoldenTree(t *testing.T) {
	dir := t.TempDir()
	created, err := Run(validConfig(dir))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	sort.Strings(created)
	if !reflect.DeepEqual(created, goldenPaths) {
		t.Fatalf("created paths mismatch:\n got  %v\n want %v", created, goldenPaths)
	}
	var found []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, dir+string(filepath.Separator)))
		found = append(found, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		wantMode := fs.FileMode(0o644)
		if rel == "make.sh" {
			wantMode = 0o755
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Errorf("%s mode %v, want %v", rel, got, wantMode)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "{{") {
			t.Errorf("%s contains an unrendered template marker", rel)
		}
		if len(b) > 0 && b[len(b)-1] != '\n' {
			t.Errorf("%s does not end with a newline", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, goldenPaths) {
		t.Fatalf("on-disk tree mismatch:\n got  %v\n want %v", found, goldenPaths)
	}
}

func TestRunRenderedJSONParses(t *testing.T) {
	for _, telegram := range []bool{true, false} {
		dir := t.TempDir()
		cfg := validConfig(dir)
		cfg.Telegram = telegram
		if _, err := Run(cfg); err != nil {
			t.Fatalf("Run(telegram=%v): %v", telegram, err)
		}
		for _, rel := range []string{"agent/spec.json", "renovate.json"} {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if !json.Valid(b) {
				t.Fatalf("%s (telegram=%v) is not valid JSON:\n%s", rel, telegram, b)
			}
		}
	}
}

func TestRunNoTelegramVariant(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig(dir)
	cfg.Telegram = false
	if _, err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	spec, err := os.ReadFile(filepath.Join(dir, "agent", "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(spec), "telegram") {
		t.Errorf("spec.json still wires telegram:\n%s", spec)
	}
	for _, rel := range []string{
		filepath.Join("agent", ".env.example"),
		filepath.Join("make.sh"),
	} {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "TELEGRAM_BOT_TOKEN") {
			t.Errorf("%s still requires TELEGRAM_BOT_TOKEN", rel)
		}
	}
}

func TestRunNonEmptyTargetWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(validConfig(dir))
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Run() = %v, want error naming --force", err)
	}
}

func TestRunForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "make.sh"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := validConfig(dir)
	cfg.Force = true
	if _, err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "make.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "junk" || !strings.Contains(string(b), "exec agentctl") {
		t.Errorf("make.sh not overwritten:\n%s", b)
	}
	info, err := os.Stat(filepath.Join(dir, "make.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("overwritten make.sh mode %v, want 0755", got)
	}
	if b, err := os.ReadFile(stale); err != nil || string(b) != "x" {
		t.Errorf("unrelated file was touched: %q, %v", b, err)
	}
}

func TestRunForceRefusesSymlinkAtManifestPath(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("outside.txt", filepath.Join(dir, "make.sh")); err != nil {
		t.Fatal(err)
	}
	cfg := validConfig(dir)
	cfg.Force = true
	_, err := Run(cfg)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Run() = %v, want non-regular-file refusal", err)
	}
	if fi, statErr := os.Lstat(filepath.Join(dir, "make.sh")); statErr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("make.sh must remain a symlink, got fi=%v err=%v", fi, statErr)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "safe" {
		t.Errorf("symlink target was overwritten through the link: %q, %v", b, err)
	}
}

func TestRunGitInit(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig(dir)
	cfg.GitInit = true
	if _, err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
		t.Errorf(".git missing after --git-init: %v", err)
	}
}

func TestMakeShPassesBashSyntaxCheck(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not in PATH")
	}
	for _, telegram := range []bool{true, false} {
		dir := t.TempDir()
		cfg := validConfig(dir)
		cfg.Telegram = telegram
		if _, err := Run(cfg); err != nil {
			t.Fatalf("Run(telegram=%v): %v", telegram, err)
		}
		if out, err := exec.Command(bash, "-n", filepath.Join(dir, "make.sh")).CombinedOutput(); err != nil {
			t.Fatalf("bash -n make.sh (telegram=%v): %v: %s", telegram, err, out)
		}
	}
}
