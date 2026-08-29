package lifecycle

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
	path, err := SecretsInit(root)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "agent", ".env") {
		t.Errorf("returned path = %q", path)
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

func TestSecretsInitMissingExample(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}"})
	if _, err := SecretsInit(root); err == nil || !strings.Contains(err.Error(), ".env.example") {
		t.Fatalf("err = %v, want .env.example read error", err)
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

func TestSecretsEdit(t *testing.T) {
	r := newFakeRunner("vi", "nvim")
	if err := SecretsEdit(r, "nvim", "/proj/agent/.env"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r, [][]string{{"nvim", "/proj/agent/.env"}})

	if err := SecretsEdit(r, "emacs", "/proj/agent/.env"); err == nil || !strings.Contains(err.Error(), "$EDITOR") {
		t.Fatalf("missing editor: err = %v, want $EDITOR hint", err)
	}

	// Empty editor falls back to vi.
	if err := SecretsEdit(r, "", "/proj/agent/.env"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls[len(r.calls)-1], " ") != "vi /proj/agent/.env" {
		t.Errorf("empty editor argv = %v, want vi", r.calls[len(r.calls)-1])
	}
}
