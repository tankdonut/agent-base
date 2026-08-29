package lifecycle

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

// SpecInfo summarizes the parts of agent/spec.json that drive env-var
// requirements: every {env:NAME} reference in any string value, the
// names appearing in any if_env array (optional by contract — guarded
// entries are skipped when the var is unset), and setup.auth_choice.
type SpecInfo struct {
	EnvRefs    []string // sorted, unique
	IfEnvNames []string // sorted, unique
	AuthChoice string
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
		if setup, ok := m["setup"].(map[string]any); ok {
			if ac, ok := setup["auth_choice"].(string); ok {
				info.AuthChoice = ac
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
	return strings.HasPrefix(s.AuthChoice, "zai-")
}
