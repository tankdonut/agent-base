package project

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// envRefRe matches {env:NAME} tokens inside spec string values.
var envRefRe = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// SpecMcpServer is one mcp_servers entry's host-side surface: the
// server name and its if_env guards. Guards that are unset at boot
// skip registration (the image's reconciliation contract), so
// registration checks must expect exactly the env-active servers.
type SpecMcpServer struct {
	Name  string
	IfEnv []string
}

// SpecInfo summarizes the parts of agent/spec.json that drive env-var
// requirements: every {env:NAME} reference in any string value, the
// names appearing in any if_env array (optional by contract — guarded
// entries are skipped when the var is unset), setup.auth_choice, the
// mcp_servers entries in spec order, and the identity fields the
// scaffold re-render recovers (agent.name, the telegram channel).
type SpecInfo struct {
	EnvRefs            []string // sorted, unique
	IfEnvNames         []string // sorted, unique
	AuthChoice         string
	McpServers         []SpecMcpServer
	AgentName          string // agent.name, "" when absent
	HasTelegramChannel bool   // channels contains {type: "telegram"}
}

// ReadSpec parses the spec at path and extracts its env surface.
func ReadSpec(path string) (SpecInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SpecInfo{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return SpecInfo{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	refs, ifEnv := map[string]bool{}, map[string]bool{}
	walkJSON(root, refs, ifEnv)
	info := SpecInfo{
		EnvRefs:    sortedNames(refs),
		IfEnvNames: sortedNames(ifEnv),
	}
	if m, ok := root.(map[string]any); ok {
		if agent, ok := m["agent"].(map[string]any); ok {
			if n, ok := agent["name"].(string); ok {
				info.AgentName = n
			}
		}
		if channels, ok := m["channels"].([]any); ok {
			for _, raw := range channels {
				entry, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := entry["type"].(string); t == "telegram" {
					info.HasTelegramChannel = true
				}
			}
		}
		if setup, ok := m["setup"].(map[string]any); ok {
			if ac, ok := setup["auth_choice"].(string); ok {
				info.AuthChoice = ac
			}
		}
		if servers, ok := m["mcp_servers"].([]any); ok {
			for _, raw := range servers {
				entry, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, ok := entry["name"].(string)
				if !ok || name == "" {
					continue
				}
				server := SpecMcpServer{Name: name}
				if guards, ok := entry["if_env"].([]any); ok {
					for _, g := range guards {
						if gs, ok := g.(string); ok && gs != "" {
							server.IfEnv = append(server.IfEnv, gs)
						}
					}
				}
				info.McpServers = append(info.McpServers, server)
			}
		}
	}
	return info, nil
}

// walkJSON collects {env:NAME} tokens from every string value and the
// element names of every if_env array, at any depth.
func walkJSON(v any, refs, ifEnv map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "if_env" {
				if arr, ok := val.([]any); ok {
					for _, e := range arr {
						if s, ok := e.(string); ok {
							ifEnv[s] = true
						}
					}
				}
			}
			walkJSON(val, refs, ifEnv)
		}
	case []any:
		for _, e := range t {
			walkJSON(e, refs, ifEnv)
		}
	case string:
		for _, m := range envRefRe.FindAllStringSubmatch(t, -1) {
			refs[m[1]] = true
		}
	}
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RequiresZAIKey reports whether the spec's auth provider load-gates on
// ZAI_API_KEY (zai-coding-*: the loader fails closed naming the var
// even though it never appears as an {env:} ref).
func (s SpecInfo) RequiresZAIKey() bool {
	return strings.HasPrefix(s.AuthChoice, "zai-coding-")
}

// RequiresLitellmKey reports whether the spec's auth provider load-gates
// on LITELLM_API_KEY (litellm-api-key: same fail-closed contract).
func (s SpecInfo) RequiresLitellmKey() bool {
	return s.AuthChoice == "litellm-api-key"
}

// AuthEnvKey returns the env var the spec's auth choice load-gates on,
// "" when none. Mirrors container/spec.py required_env_for_auth_choice
// so agentctl's validate and secrets surfaces never drift from the
// image loader's gate.
func (s SpecInfo) AuthEnvKey() string {
	switch {
	case s.RequiresZAIKey():
		return "ZAI_API_KEY"
	case s.RequiresLitellmKey():
		return "LITELLM_API_KEY"
	}
	return ""
}
