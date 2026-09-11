package project

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const envExampleWithToken = `# contract
#OPENCLAW_GATEWAY_TOKEN=
#ZAI_API_KEY=
#TELEGRAM_ALLOWED_USERS=
`

func TestSecretsInit(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/.env.example": envExampleWithToken})
	paths, err := SecretsInit(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join(root, "agent", ".env") {
		t.Errorf("returned paths = %v", paths)
	}
	data, err := os.ReadFile(filepath.Join(root, "agent", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// The commented token line is replaced with a 64-hex value.
	re := regexp.MustCompile(`(?m)^OPENCLAW_GATEWAY_TOKEN=([0-9a-f]{64})$`)
	m := re.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("OPENCLAW_GATEWAY_TOKEN not set to 64-hex value:\n%s", content)
	}
	if strings.Contains(content, "#OPENCLAW_GATEWAY_TOKEN=") {
		t.Error("commented token line still present after replace")
	}
	if !strings.Contains(content, "#ZAI_API_KEY=") {
		t.Error("unrelated commented lines must be preserved")
	}

	// Mode 0600, and the example file is untouched.
	fi, err := os.Stat(filepath.Join(root, "agent", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}
	example, _ := os.ReadFile(filepath.Join(root, "agent", ".env.example"))
	if string(example) != envExampleWithToken {
		t.Error("agent/.env.example was modified")
	}

	// Tokens are random: two inits differ (checked via distinct fixtures).
	other := writeProject(t, map[string]string{"agent/.env.example": envExampleWithToken})
	if _, err := SecretsInit(other); err != nil {
		t.Fatal(err)
	}
	otherData, _ := os.ReadFile(filepath.Join(other, "agent", ".env"))
	if m2 := re.FindStringSubmatch(string(otherData)); m2 != nil && m2[1] == m[1] {
		t.Error("two inits produced identical tokens")
	}
}

const litellmEnvExample = `# LiteLLM proxy secrets — provider keys live ONLY here; the agent
# container never sees this file.
#LITELLM_MASTER_KEY=
#OPENAI_API_KEY=
`

func TestSecretsInitLitellm(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/.env.example":   envExampleWithToken,
		"litellm/.env.example": litellmEnvExample,
	})
	paths, err := SecretsInit(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want agent/.env and litellm/.env", paths)
	}
	if paths[0] != filepath.Join(root, "agent", ".env") || paths[1] != filepath.Join(root, "litellm", ".env") {
		t.Fatalf("paths = %v", paths)
	}
	aenv, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	lenv, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}

	masterRe := regexp.MustCompile(`(?m)^LITELLM_MASTER_KEY=(sk-[0-9a-f]{48})$`)
	m := masterRe.FindStringSubmatch(string(lenv))
	if m == nil {
		t.Fatalf("LITELLM_MASTER_KEY not set to sk-<48hex>:\n%s", lenv)
	}
	if !strings.Contains(string(aenv), "LITELLM_API_KEY="+m[1]) {
		t.Errorf("agent/.env LITELLM_API_KEY does not mirror the master key:\n%s", aenv)
	}
	if strings.Contains(string(aenv), "OPENAI_API_KEY=") {
		t.Error("provider keys must never leak into agent/.env")
	}
	if !strings.Contains(string(aenv), "#ZAI_API_KEY=") {
		t.Error("unrelated commented lines must be preserved")
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, fi.Mode().Perm())
		}
	}
}

func TestSecretsInitLitellmRefusals(t *testing.T) {
	fixtures := map[string]string{
		"agent/.env.example":   envExampleWithToken,
		"litellm/.env.example": litellmEnvExample,
		"litellm/.env":         "LITELLM_MASTER_KEY=sk-existing\n",
	}
	root := writeProject(t, fixtures)
	_, err := SecretsInit(root)
	if err == nil || !strings.Contains(err.Error(), "secrets edit") {
		t.Fatalf("err = %v, want refusal pointing at secrets edit", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agent", ".env")); !os.IsNotExist(err) {
		t.Error("agent/.env must not be created when litellm/.env already exists")
	}
	existing, _ := os.ReadFile(filepath.Join(root, "litellm", ".env"))
	if string(existing) != "LITELLM_MASTER_KEY=sk-existing\n" {
		t.Error("existing litellm/.env must not be touched")
	}

	linkRoot := writeProject(t, map[string]string{
		"agent/.env.example":   envExampleWithToken,
		"litellm/.env.example": litellmEnvExample,
	})
	if err := os.Symlink("../outside-litellm.env", filepath.Join(linkRoot, "litellm", ".env")); err != nil {
		t.Fatal(err)
	}
	if _, err := SecretsInit(linkRoot); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want symlink refusal", err)
	}
	if _, err := os.Stat(filepath.Join(linkRoot, "outside-litellm.env")); !os.IsNotExist(err) {
		t.Error("init wrote through the dangling symlink")
	}
}

func TestSecretsInitAppendsWhenCommentedLineAbsent(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/.env.example": "#A=1\n"})
	if _, err := SecretsInit(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "agent", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^OPENCLAW_GATEWAY_TOKEN=[0-9a-f]{64}$`)
	if !re.MatchString(string(data)) {
		t.Fatalf("token var not appended:\n%s", data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("file must end with a newline")
	}
}

func TestSecretsInitRefusesExistingEnv(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/.env.example": envExampleWithToken,
		"agent/.env":         "EXISTING=1\n",
	})
	_, err := SecretsInit(root)
	if err == nil || !strings.Contains(err.Error(), "secrets edit") {
		t.Fatalf("err = %v, want refusal pointing at secrets edit", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "agent", ".env"))
	if string(data) != "EXISTING=1\n" {
		t.Error("existing agent/.env must not be touched")
	}
}

func TestSecretsInitRefusesSymlinkAtEnvPath(t *testing.T) {
	// A dangling symlink (committable to git) must not be written
	// through: O_EXCL creation fails and the target stays absent.
	root := writeProject(t, map[string]string{"agent/.env.example": envExampleWithToken})
	link := filepath.Join(root, "agent", ".env")
	if err := os.Symlink("../outside.env", link); err != nil {
		t.Fatal(err)
	}
	if _, err := SecretsInit(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want symlink refusal", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("env path must remain a symlink, got fi=%v err=%v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(root, "outside.env")); !os.IsNotExist(err) {
		t.Error("init wrote through the dangling symlink")
	}

	// A symlink with an existing target is refused just as loudly.
	root2 := writeProject(t, map[string]string{
		"agent/.env.example": envExampleWithToken,
		"agent/.env.target":  "PREEXISTING=1\n",
	})
	if err := os.Symlink(".env.target", filepath.Join(root2, "agent", ".env")); err != nil {
		t.Fatal(err)
	}
	if _, err := SecretsInit(root2); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want symlink refusal for existing target", err)
	}
	data, _ := os.ReadFile(filepath.Join(root2, "agent", ".env.target"))
	if string(data) != "PREEXISTING=1\n" {
		t.Error("symlink target was modified through the link")
	}
}

func TestSecretsInitMissingExample(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}"})
	if _, err := SecretsInit(root); err == nil || !strings.Contains(err.Error(), ".env.example") {
		t.Fatalf("err = %v, want .env.example read error", err)
	}
}

// TestEnvKeyNamesNeverLeaksValues locks the names-only contract of
// EnvKeyNames: a value planted in agent/.env must never travel with the
// key list (the Deployment IR prints it). The argv-level canary over
// the compose verbs lives in internal/compose.
func TestEnvKeyNamesNeverLeaksValues(t *testing.T) {
	const canary = "CANARY-7f3a9d1c-value"
	root := writeProject(t, map[string]string{
		"agent/spec.json": fixtureSpec,
		"agent/.env":      "FALLBACK_MODEL=m\nPROVIDER_KEY=" + canary + "\nZAI_API_KEY=" + canary + "\n",
	})
	names, err := EnvKeyNames(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.Contains(name, canary) {
			t.Fatalf("EnvKeyNames leaks a value-shaped name: %q", name)
		}
	}
	if len(names) != 3 {
		t.Errorf("names = %v, want the three set keys", names)
	}
}

func TestSecretsCheck(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		env     string
		wantN   int
		wantErr string
	}{
		{
			name:  "required vars set",
			spec:  fixtureSpec,
			env:   "FALLBACK_MODEL=m\nPROVIDER_KEY=k\nZAI_API_KEY=z\n",
			wantN: 3, // GUARDED ref excluded via if_env; ZAI added by auth gate
		},
		{
			name:    "missing vars all listed",
			spec:    fixtureSpec,
			env:     "FALLBACK_MODEL=m\n",
			wantErr: "PROVIDER_KEY, ZAI_API_KEY",
		},
		{
			name:    "empty value counts as missing",
			spec:    fixtureSpec,
			env:     "FALLBACK_MODEL=m\nPROVIDER_KEY=\nZAI_API_KEY=z\n",
			wantErr: "PROVIDER_KEY",
		},
		{
			name:  "if_env-guarded var is optional",
			spec:  `{"config": [{"path": "x", "value": "{env:OPT}", "if_env": ["OPT"]}]}`,
			env:   "UNRELATED=1\n",
			wantN: 0,
		},
		{
			name:  "export prefix and comments ignored",
			spec:  `{"config": [{"path": "x", "value": "{env:FOO}"}]}`,
			env:   "#FOO=nope\nexport FOO=bar\n",
			wantN: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProject(t, map[string]string{
				"agent/spec.json": tt.spec,
				"agent/.env":      tt.env,
			})
			n, err := SecretsCheck(root)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want listing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.wantN {
				t.Errorf("checked = %d, want %d", n, tt.wantN)
			}
		})
	}
}

func TestSecretsCheckMissingEnvFile(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}"})
	if _, err := SecretsCheck(root); err == nil || !strings.Contains(err.Error(), "secrets init") {
		t.Fatalf("err = %v, want pointer at secrets init", err)
	}
}

const litellmSpec = `{
  "specVersion": 1,
  "setup": {"auth_choice": "litellm-api-key"},
  "model": {"fallback": "litellm/glm-5.2"},
  "config": [],
  "mcp_servers": []
}`

func TestSecretsCheckLitellm(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantN   int
		wantErr string
	}{
		{
			name: "mirrored keys pass",
			files: map[string]string{
				"agent/spec.json":      litellmSpec,
				"agent/.env":           "LITELLM_API_KEY=sk-same\n",
				"litellm/.env.example": litellmEnvExample,
				"litellm/.env":         "LITELLM_MASTER_KEY=sk-same\n",
			},
			wantN: 1,
		},
		{
			name: "mismatch names both vars, never values",
			files: map[string]string{
				"agent/spec.json":      litellmSpec,
				"agent/.env":           "LITELLM_API_KEY=sk-agent-side\n",
				"litellm/.env.example": litellmEnvExample,
				"litellm/.env":         "LITELLM_MASTER_KEY=sk-proxy-side\n",
			},
			wantErr: "LITELLM_API_KEY (agent/.env) does not match LITELLM_MASTER_KEY (litellm/.env)",
		},
		{
			name: "missing master key",
			files: map[string]string{
				"agent/spec.json":      litellmSpec,
				"agent/.env":           "LITELLM_API_KEY=sk-same\n",
				"litellm/.env.example": litellmEnvExample,
				"litellm/.env":         "#LITELLM_MASTER_KEY=\n",
			},
			wantErr: "missing or empty in litellm/.env: LITELLM_MASTER_KEY",
		},
		{
			name: "missing litellm/.env with example present",
			files: map[string]string{
				"agent/spec.json":      litellmSpec,
				"agent/.env":           "LITELLM_API_KEY=sk-same\n",
				"litellm/.env.example": litellmEnvExample,
			},
			wantErr: "litellm/.env not found",
		},
		{
			name: "mismatch error never carries key values",
			files: map[string]string{
				"agent/spec.json":      litellmSpec,
				"agent/.env":           "LITELLM_API_KEY=sk-agent-side\n",
				"litellm/.env.example": litellmEnvExample,
				"litellm/.env":         "LITELLM_MASTER_KEY=sk-proxy-side\n",
			},
			wantErr: "does not match",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProject(t, tt.files)
			n, err := SecretsCheck(root)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "sk-agent-side") || strings.Contains(err.Error(), "sk-proxy-side") {
					t.Fatalf("error leaks key values: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.wantN {
				t.Errorf("checked = %d, want %d", n, tt.wantN)
			}
		})
	}
}

func TestSecretsCheckLegacyProjectWithoutLitellm(t *testing.T) {
	// Projects without litellm/.env.example keep the single-file contract.
	root := writeProject(t, map[string]string{
		"agent/spec.json": fixtureSpec,
		"agent/.env":      "FALLBACK_MODEL=m\nPROVIDER_KEY=k\nZAI_API_KEY=z\n",
	})
	n, err := SecretsCheck(root)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("checked = %d, want 3", n)
	}
}
