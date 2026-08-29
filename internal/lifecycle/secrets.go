package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// gatewayTokenVar is set by secrets init with a generated value.
const gatewayTokenVar = "OPENCLAW_GATEWAY_TOKEN"

// GenerateToken returns a 32-byte crypto/rand value as 64 hex chars.
// No openssl subprocess: it may be absent in minimal environments.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SecretsInit creates agent/.env from agent/.env.example (refusing to
// overwrite an existing file), sets OPENCLAW_GATEWAY_TOKEN to a
// generated 64-hex value by replacing the commented template line (or
// appending the var when that line is absent), and chmods the file
// 0600.
func SecretsInit(root string) (string, error) {
	envPath := filepath.Join(root, "agent", ".env")
	if fi, err := os.Lstat(envPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s already exists and is a symlink — inspect it, then run `agentctl secrets edit`", envPath)
		}
		return "", fmt.Errorf("%s already exists — run `agentctl secrets edit` to change values", envPath)
	}
	data, err := os.ReadFile(filepath.Join(root, "agent", ".env.example"))
	if err != nil {
		return "", fmt.Errorf("reading agent/.env.example: %w", err)
	}
	token, err := GenerateToken()
	if err != nil {
		return "", err
	}
	out := setGatewayToken(string(data), token)
	// O_EXCL creation never follows a symlink planted at envPath (a
	// dangling symlink yields EEXIST too), so a hostile clone cannot
	// turn init into an arbitrary-path write.
	f, err := os.OpenFile(envPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%s already exists — run `agentctl secrets edit` to change values", envPath)
		}
		return "", fmt.Errorf("creating agent/.env: %w", err)
	}
	if _, err := f.WriteString(out); err != nil {
		f.Close()
		return "", fmt.Errorf("writing agent/.env: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing agent/.env: %w", err)
	}
	// chmod keeps 0600 explicit even under a permissive umask.
	if err := os.Chmod(envPath, 0o600); err != nil {
		return "", err
	}
	return envPath, nil
}

// setGatewayToken line-edits the template: replace the commented
// `#OPENCLAW_GATEWAY_TOKEN=` line (first match) with the active var, or
// append the var when the commented line is absent.
func setGatewayToken(tmpl, token string) string {
	commented := "#" + gatewayTokenVar + "="
	lines := strings.Split(tmpl, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), commented) {
			lines[i] = gatewayTokenVar + "=" + token
			return strings.Join(lines, "\n")
		}
	}
	out := tmpl
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + gatewayTokenVar + "=" + token + "\n"
}

// parseEnvValues extracts non-comment NAME=VALUE pairs (ignoring an
// optional `export ` prefix) from dotenv content. Duplicate names keep
// the last occurrence, matching dotenv semantics.
func parseEnvValues(data string) map[string]string {
	m := map[string]string{}
	for _, ln := range strings.Split(data, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		ln = strings.TrimPrefix(ln, "export ")
		name, val, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(name)] = strings.TrimSpace(val)
	}
	return m
}

// RequiredEnvVars derives the vars agent/.env must set: every {env:NAME}
// ref in spec.json, minus names guarded by if_env (optional by
// contract), plus ZAI_API_KEY when the auth provider requires it.
func RequiredEnvVars(info SpecInfo) []string {
	guarded := map[string]bool{}
	for _, n := range info.IfEnvNames {
		guarded[n] = true
	}
	required := make([]string, 0, len(info.EnvRefs))
	for _, n := range info.EnvRefs {
		if !guarded[n] {
			required = append(required, n)
		}
	}
	if info.RequiresZAIKey() && !contains(required, "ZAI_API_KEY") {
		required = append(required, "ZAI_API_KEY")
	}
	sort.Strings(required)
	return required
}

// SecretsCheck verifies every required var in agent/.env is set and
// non-empty. On success it returns the count of vars checked; on
// failure the error lists every offender.
func SecretsCheck(root string) (int, error) {
	envPath := filepath.Join(root, "agent", ".env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		return 0, fmt.Errorf("agent/.env not found — run `agentctl secrets init` first")
	}
	info, err := ReadSpec(filepath.Join(root, "agent", "spec.json"))
	if err != nil {
		return 0, err
	}
	set := parseEnvValues(string(data))
	required := RequiredEnvVars(info)
	var missing []string
	for _, n := range required {
		if v, ok := set[n]; !ok || v == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("missing or empty in agent/.env: %s", strings.Join(missing, ", "))
	}
	return len(required), nil
}

// SecretsEdit opens agent/.env in the user's editor.
func SecretsEdit(r Runner, editor, envPath string) error {
	if r == nil {
		return errNilRunner
	}
	if editor == "" {
		editor = "vi"
	}
	if _, err := lookPath(r, editor); err != nil {
		return fmt.Errorf("editor %q not found in PATH (set $EDITOR)", editor)
	}
	return runArgv(r, nil, editor, envPath)
}
