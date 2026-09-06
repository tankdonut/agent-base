// config.go loads agentctl's typed configuration: defaults, then an
// optional .agentctl.yaml (working directory, else the project root),
// then AGENTCTL_* env vars. The file is validated fail-closed: unknown
// top-level keys abort with rename hints for the legacy flat names.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

// ConfigName is the per-project agentctl config file.
const ConfigName = ".agentctl.yaml"

// Config is the loaded configuration. Platform names the pinned
// deployment platform (registry key; "compose" default); Namespaces
// carries each platform's config namespace verbatim — platform
// adapters decode their own typed config from their map and fail
// closed on keys they do not know.
type Config struct {
	Platform   string
	Namespaces map[string]map[string]any
}

// LoadConfig layers defaults < file < env. Cheap enough to call per
// command; no global state.
func LoadConfig() (Config, error) {
	cfg := Config{Platform: "compose", Namespaces: map[string]map[string]any{}}
	path := agentctlConfigPath()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("reading %s: %w", path, err)
		}
		if err := applyConfigFile(&cfg, data, path); err != nil {
			return cfg, err
		}
	}
	if err := applyConfigEnv(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// legacyKeys maps the pre-platform flat config keys to their new homes,
// for actionable unknown-key errors.
var legacyKeys = map[string]string{
	"engine":       "compose.engine",
	"gateway_port": "compose.gateway_port",
}

// applyConfigFile decodes and validates .agentctl.yaml. Top-level keys
// are strict (platform, compose); namespace contents are the owning
// adapter's contract, checked when the adapter is constructed.
func applyConfigFile(cfg *Config, data []byte, path string) error {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%s: parsing YAML: %w", path, err)
	}
	for key, val := range raw {
		switch key {
		case "platform":
			s, ok := val.(string)
			if !ok {
				return fmt.Errorf("%s: platform: want a string, got %T", path, val)
			}
			if s == "" {
				return fmt.Errorf("%s: platform is empty — remove the key (default: compose) or set a registry name", path)
			}
			cfg.Platform = s
		case "compose":
			ns, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: compose: want a mapping of compose config keys, got %T", path, val)
			}
			cfg.Namespaces["compose"] = ns
		default:
			if renamed, legacy := legacyKeys[key]; legacy {
				return fmt.Errorf("%s: unknown key %q — it moved: rename it to %s", path, key, renamed)
			}
			return fmt.Errorf("%s: unknown key %q (known: platform, compose)", path, key)
		}
	}
	return nil
}

// applyConfigEnv overlays AGENTCTL_PLATFORM, AGENTCTL_COMPOSE_ENGINE,
// and AGENTCTL_COMPOSE_GATEWAY_PORT. A malformed value is an error, not
// a silent skip.
func applyConfigEnv(cfg *Config) error {
	if v := os.Getenv("AGENTCTL_PLATFORM"); v != "" {
		cfg.Platform = v
	}
	ns := cfg.Namespaces["compose"]
	if ns == nil {
		ns = map[string]any{}
		cfg.Namespaces["compose"] = ns
	}
	if v := os.Getenv("AGENTCTL_COMPOSE_ENGINE"); v != "" {
		ns["engine"] = v
	}
	if v := os.Getenv("AGENTCTL_COMPOSE_GATEWAY_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("AGENTCTL_COMPOSE_GATEWAY_PORT: want an integer, got %q", v)
		}
		ns["gateway_port"] = n
	}
	return nil
}

// ComposeEnginePref extracts compose.engine from the loaded namespaces
// ("" when unset — ResolveEngine treats it as auto).
func (c Config) ComposeEnginePref() string {
	if s, ok := c.Namespaces["compose"]["engine"].(string); ok {
		return s
	}
	return ""
}

// ComposeGatewayPort extracts compose.gateway_port, falling back to
// the adapter default.
func (c Config) ComposeGatewayPort() int {
	if n, ok := c.Namespaces["compose"]["gateway_port"].(int); ok && n != 0 {
		return n
	}
	return 18789
}

// agentctlConfigPath resolves the config file: a .agentctl.yaml in the
// working directory wins; otherwise the project root's copy — commands
// resolve the project by marker from any subdirectory, so the config
// must follow the same root, not the invocation cwd.
func agentctlConfigPath() string {
	if _, err := os.Stat(ConfigName); err == nil {
		return ConfigName
	}
	root, err := lifecycle.FindProjectRoot(".")
	if err != nil {
		return "" // not inside a project: defaults + env only
	}
	candidate := filepath.Join(root, ConfigName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// pinPlatform writes `platform: <name>` into the project's
// .agentctl.yaml, replacing an existing platform line in place or
// appending one. Line-oriented on purpose: the scaffolded file is
// comment-heavy and must survive intact.
func pinPlatform(root, name string) error {
	path := filepath.Join(root, ConfigName)
	data, err := os.ReadFile(path)
	replace := err == nil
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	line := "platform: " + name
	if replace {
		lines := strings.Split(string(data), "\n")
		found := false
		for i, ln := range lines {
			if strings.HasPrefix(strings.TrimSpace(ln), "platform:") {
				lines[i] = line
				found = true
				break
			}
		}
		if !found {
			lines = append(lines, line)
		}
		data = []byte(strings.Join(lines, "\n"))
	} else {
		data = []byte("# agentctl config — see `agentctl platform ls`\n" + line + "\n")
	}
	return os.WriteFile(path, data, 0o644)
}
