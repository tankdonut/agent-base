package project

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

const (
	gatewayTokenVar = "OPENCLAW_GATEWAY_TOKEN"
	litellmKeyVar   = "LITELLM_API_KEY"
	masterKeyVar    = "LITELLM_MASTER_KEY"
)

// GenerateToken returns a 32-byte crypto/rand value as 64 hex chars.
// No openssl subprocess: it may be absent in minimal environments.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// GenerateMasterKey returns a LiteLLM proxy master key: "sk-" + 24-byte
// crypto/rand hex. The sk- prefix is the format LiteLLM requires of
// authenticated keys.
func GenerateMasterKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating master key: %w", err)
	}
	return "sk-" + hex.EncodeToString(buf), nil
}

// SecretsInit creates agent/.env from agent/.env.example (refusing to
// overwrite an existing file), sets OPENCLAW_GATEWAY_TOKEN to a
// generated 64-hex value by replacing the commented template line (or
// appending the var when that line is absent), and — when the project
// ships a LiteLLM sidecar (litellm/.env.example present) — additionally
// creates litellm/.env with a generated sk- master key, mirroring that
// key into agent/.env as LITELLM_API_KEY (the only key the agent ever
// holds; provider keys live solely in litellm/.env). Every file lands
// 0600; the returned paths list what was written.
func SecretsInit(root string) ([]string, error) {
	envPath := filepath.Join(root, "agent", ".env")
	if err := refuseExisting(envPath); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "agent", ".env.example"))
	if err != nil {
		return nil, fmt.Errorf("reading agent/.env.example: %w", err)
	}
	token, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	out := setEnvVar(string(data), gatewayTokenVar, token)

	written := []string{envPath}
	var master string
	if _, err := os.Stat(filepath.Join(root, "litellm", ".env.example")); err == nil {
		if err := refuseExisting(filepath.Join(root, "litellm", ".env")); err != nil {
			return nil, err
		}
		master, err = GenerateMasterKey()
		if err != nil {
			return nil, err
		}
		out = setEnvVar(out, litellmKeyVar, master)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if err := writeEnvExclusive(envPath, out); err != nil {
		return nil, err
	}
	if master != "" {
		ldata, lerr := os.ReadFile(filepath.Join(root, "litellm", ".env.example"))
		if lerr != nil {
			return nil, fmt.Errorf("reading litellm/.env.example: %w", lerr)
		}
		lpath := filepath.Join(root, "litellm", ".env")
		if lerr := writeEnvExclusive(lpath, setEnvVar(string(ldata), masterKeyVar, master)); lerr != nil {
			return nil, lerr
		}
		written = append(written, lpath)
	}
	return written, nil
}

// refuseExisting rejects a pre-existing secrets file, calling out
// symlinks specifically (a planted link must never be written through).
func refuseExisting(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s already exists and is a symlink — inspect it, then run `agentctl secrets edit`", path)
	}
	return fmt.Errorf("%s already exists — run `agentctl secrets edit` to change values", path)
}

// writeEnvExclusive creates path with content via O_EXCL creation —
// never following a symlink planted at path (a dangling link yields
// EEXIST too, so a hostile clone cannot turn init into an
// arbitrary-path write) — and chmods 0600 even under a permissive umask.
func writeEnvExclusive(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists — run `agentctl secrets edit` to change values", path)
		}
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return nil
}

// setEnvVar line-edits a dotenv template: replace the commented
// `#NAME=` line (first match) with the active var, or append the var
// when the commented line is absent.
func setEnvVar(tmpl, name, value string) string {
	commented := "#" + name + "="
	lines := strings.Split(tmpl, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), commented) {
			lines[i] = name + "=" + value
			return strings.Join(lines, "\n")
		}
	}
	out := tmpl
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + name + "=" + value + "\n"
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
// contract), plus the auth-gated key when the auth provider requires
// one (ZAI_API_KEY / LITELLM_API_KEY).
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
	if k := info.AuthEnvKey(); k != "" && !contains(required, k) {
		required = append(required, k)
	}
	sort.Strings(required)
	return required
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// EnvKeyNames returns the sorted variable names set in agent/.env, nil
// when the file is absent. Names only — values never leave the file —
// so callers can print the result (platform.Deployment.EnvKeys).
func EnvKeyNames(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "agent", ".env"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading agent/.env: %w", err)
	}
	set := parseEnvValues(string(data))
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// SecretsCheck verifies every required var in agent/.env is set and
// non-empty. Projects shipping a litellm sidecar (litellm/.env.example)
// must additionally set LITELLM_MASTER_KEY in litellm/.env, and when
// the agent carries LITELLM_API_KEY too, the two must match (compared
// here, never printed). On success it returns the count of agent-side
// vars checked; on failure the error lists every offender.
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
	if _, err := os.Stat(filepath.Join(root, "litellm", ".env.example")); err == nil {
		ldata, lerr := os.ReadFile(filepath.Join(root, "litellm", ".env"))
		if lerr != nil {
			return 0, fmt.Errorf("litellm/.env not found — run `agentctl secrets init` first")
		}
		lset := parseEnvValues(string(ldata))
		master, ok := lset[masterKeyVar]
		if !ok || master == "" {
			return 0, fmt.Errorf("missing or empty in litellm/.env: %s", masterKeyVar)
		}
		if client, ok := set[litellmKeyVar]; ok && client != "" && client != master {
			return 0, fmt.Errorf(
				"%s (agent/.env) does not match %s (litellm/.env) — rotate them together",
				litellmKeyVar,
				masterKeyVar,
			)
		}
	}
	return len(required), nil
}
