// Package fleet loads the fleet manifest (fleet.yaml) — the single
// authored deployment-shape source for every agentctl repo, whether a
// fleet of one or a monorepo. The manifest owns the roster (agents/
// services registries), port allocation, platform routing, and the
// plane (shared services) block; per-agent compose files are RENDERED
// from it, never authored.
//
// The loader is fail-closed like every agentctl loader: unknown keys
// abort with the known set (rename hints where a legacy .agentctl.yaml
// key was pasted in), ambiguous shapes never resolve silently, and
// ports allocate deterministically (sorted agent names — never YAML
// document order, which reordering edits would silently change).
package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ManifestName is the fleet manifest file identifying a repo root.
const ManifestName = "fleet.yaml"

// AgentsDir is the canonical per-agent directory under the fleet root.
const AgentsDir = "agents"

// ServicesDir is the canonical fleet-service directory under the root.
const ServicesDir = "services"

// DefaultGatewayBasePort is the first host port allocated to agent
// gateways when plane.gateway_base_port is unset (openclaw's own
// default, kept for the fleet-of-one).
const DefaultGatewayBasePort = 18789

// nameRE accepts registry keys that are also safe directory names and
// compose project names (mirrors the spec preset-name grammar).
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// LiteLLM placement values.
const (
	LiteLLMShared  = "shared"  // the plane's shared proxy
	LiteLLMSidecar = "sidecar" // per-agent proxy (today's shape)
	LiteLLMNone    = "none"    // plane variant: no shared proxy at all
)

// Manifest is a fully validated fleet.yaml. Registry keys are identity:
// an agent's directory is always <root>/agents/<key>, a service's
// <root>/services/<key> unless dir is explicit.
type Manifest struct {
	Root     string
	Path     string
	Plane    Plane
	Defaults Defaults
	Agents   map[string]AgentEntry
	Services map[string]ServiceEntry
}

// Plane is the shared-services block. Disabled (the fresh fleet-of-one
// default) means every agent runs its own litellm sidecar and nothing
// is shared.
type Plane struct {
	Enabled         bool
	Name            string
	GatewayBasePort int
	LiteLLM         string // shared (default) | none
	Observability   bool
}

// Defaults carries fleet-wide values merged into every agent at render
// time (effective-spec composition happens in the renderer; the loader
// only validates shape).
type Defaults struct {
	ComposeEngine string
	Plugins       []PluginRef
	McpServers    []McpEntry
}

// PluginRef mirrors the spec's plugins[] entry: name, plus an optional
// source ("" = the plugin registry).
type PluginRef struct {
	Name   string
	Source string
}

// McpEntry is a defaults.mcp_servers[] entry. Only the identity and the
// stdio/remote invariant are checked here; the raw mapping is carried
// verbatim for the renderer to splice — the container's own spec loader
// remains the deep validator (frozen image contract, not duplicated).
type McpEntry struct {
	Name string
	Raw  map[string]any
}

// AgentEntry is one roster entry. GatewayPort is always resolved after
// load (explicit or allocated); ExplicitPort records which.
type AgentEntry struct {
	Name         string
	Platform     string // "" = compose (registry default)
	GatewayPort  int
	ExplicitPort bool
	LiteLLM      string // "" | shared | sidecar
	LiteLLMUsed  string // resolved: shared when the plane provides it, else sidecar
	FlyApp       string
	FlyRegion    string
	Dir          string // absolute
	ComposeFile  string // authored opt-out: rel path under the agent dir replacing the render
	Overrides    *Overrides
}

// Overrides is the bounded per-agent render delta: extra volume
// mounts and port publishes append; limits replace the resource
// ceiling wholesale.
type Overrides struct {
	Volumes []string
	Ports   []string
	Limits  *ResourceLimits
}

// ResourceLimits replaces deploy.resources.limits when overridden.
type ResourceLimits struct {
	Cpus   string
	Memory string
	Pids   int
}

// ServiceEntry is one fleet-managed external service (e.g. an MCP
// server too heavy to co-locate with an agent).
type ServiceEntry struct {
	Name     string
	Dir      string // absolute
	Networks []string
}

// AgentNames lists registry keys in deterministic (sorted) order.
func (m *Manifest) AgentNames() []string {
	names := make([]string, 0, len(m.Agents))
	for name := range m.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ServiceNames lists service registry keys in sorted order.
func (m *Manifest) ServiceNames() []string {
	names := make([]string, 0, len(m.Services))
	for name := range m.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FindRoot walks up from start until it finds a directory containing
// fleet.yaml and returns it as an absolute path. The error names the
// missing marker when no ancestor qualifies.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", start, err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ManifestName)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found in %s or any parent — run inside an agentctl fleet repo (see `agentctl init`)", ManifestName, start)
		}
		dir = parent
	}
}

// legacyTopLevel maps pasted .agentctl.yaml top-level keys to their
// fleet.yaml homes, for actionable unknown-key errors.
var legacyTopLevel = map[string]string{
	"platform": "agents.<name>.platform",
	"compose":  "defaults.compose.engine / plane.gateway_base_port / agents.<name>.gateway_port",
	"fly":      "agents.<name>.fly",
}

var topLevelKeys = map[string]bool{
	"plane":    true,
	"defaults": true,
	"services": true,
	"agents":   true,
}

// LoadManifest reads and validates <root>/fleet.yaml. Every failure
// names the manifest path and the dotted key path of the offense.
func LoadManifest(root string) (*Manifest, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", root, err)
	}
	path := filepath.Join(abs, ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: parsing YAML: %w", path, err)
	}
	for key, val := range raw {
		normalized, err := stringifyMaps(val)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		}
		raw[key] = normalized
	}
	m := &Manifest{
		Root:     abs,
		Path:     path,
		Agents:   map[string]AgentEntry{},
		Services: map[string]ServiceEntry{},
		Plane:    Plane{GatewayBasePort: DefaultGatewayBasePort, LiteLLM: LiteLLMShared, Observability: true},
	}
	for key, val := range raw {
		switch key {
		case "plane":
			if err := applyPlane(&m.Plane, val, path); err != nil {
				return nil, err
			}
		case "defaults":
			if err := applyDefaults(&m.Defaults, val, path); err != nil {
				return nil, err
			}
		case "services":
			if err := applyServices(m, val, path); err != nil {
				return nil, err
			}
		case "agents":
			if err := applyAgents(m, val, path); err != nil {
				return nil, err
			}
		default:
			if renamed, legacy := legacyTopLevel[key]; legacy {
				return nil, fmt.Errorf("%s: unknown key %q — it moved: fleet.yaml does not take %s per-repo config; rename it to %s", path, key, key, renamed)
			}
			return nil, fmt.Errorf("%s: unknown key %q (known: agents, defaults, plane, services)", path, key)
		}
	}
	if len(m.Agents) == 0 {
		return nil, fmt.Errorf("%s: no agents registered — a fleet of one still lists its single agent under agents:", path)
	}
	if err := m.allocatePorts(); err != nil {
		return nil, err
	}
	if err := m.resolveLiteLLM(); err != nil {
		return nil, err
	}
	return m, nil
}

func applyPlane(plane *Plane, val any, path string) error {
	node, err := expectMapping(val, "plane", path)
	if err != nil {
		return err
	}
	for key, v := range node {
		switch key {
		case "enabled":
			b, err := expectBool(v, "plane.enabled", path)
			if err != nil {
				return err
			}
			plane.Enabled = b
		case "name":
			s, err := expectString(v, "plane.name", path)
			if err != nil {
				return err
			}
			plane.Name = s
		case "gateway_base_port":
			n, err := expectPort(v, "plane.gateway_base_port", path)
			if err != nil {
				return err
			}
			plane.GatewayBasePort = n
		case "litellm":
			s, err := expectString(v, "plane.litellm", path)
			if err != nil {
				return err
			}
			if s != LiteLLMShared && s != LiteLLMNone {
				return fmt.Errorf("%s: plane.litellm: want %q or %q, got %q", path, LiteLLMShared, LiteLLMNone, s)
			}
			plane.LiteLLM = s
		case "observability":
			b, err := expectBool(v, "plane.observability", path)
			if err != nil {
				return err
			}
			plane.Observability = b
		default:
			return fmt.Errorf("%s: plane: unknown key %q (known: enabled, gateway_base_port, litellm, name, observability)", path, key)
		}
	}
	if plane.Enabled && plane.Name == "" {
		return fmt.Errorf("%s: plane.name is required when plane.enabled is true — it names the shared-services compose project", path)
	}
	if plane.Enabled && nameRE.MatchString(plane.Name) == false {
		return fmt.Errorf("%s: plane.name: %q is not a valid compose project name (lowercase letters, digits, - _)", path, plane.Name)
	}
	return nil
}

func applyDefaults(defs *Defaults, val any, path string) error {
	node, err := expectMapping(val, "defaults", path)
	if err != nil {
		return err
	}
	for key, v := range node {
		switch key {
		case "compose":
			comp, err := expectMapping(v, "defaults.compose", path)
			if err != nil {
				return err
			}
			for ck, cv := range comp {
				switch ck {
				case "engine":
					s, err := expectString(cv, "defaults.compose.engine", path)
					if err != nil {
						return err
					}
					defs.ComposeEngine = s
				default:
					return fmt.Errorf("%s: defaults.compose: unknown key %q (known: engine)", path, ck)
				}
			}
		case "plugins":
			list, err := expectList(v, "defaults.plugins", path)
			if err != nil {
				return err
			}
			for i, item := range list {
				base := fmt.Sprintf("defaults.plugins[%d]", i)
				pn, err := expectMapping(item, base, path)
				if err != nil {
					return err
				}
				ref := PluginRef{}
				for pk, pv := range pn {
					switch pk {
					case "name":
						s, err := expectString(pv, base+".name", path)
						if err != nil {
							return err
						}
						ref.Name = s
					case "source":
						s, err := expectString(pv, base+".source", path)
						if err != nil {
							return err
						}
						ref.Source = s
					default:
						return fmt.Errorf("%s: %s: unknown key %q (known: name, source)", path, base, pk)
					}
				}
				if ref.Name == "" {
					return fmt.Errorf("%s: %s: name is required", path, base)
				}
				defs.Plugins = append(defs.Plugins, ref)
			}
		case "mcp_servers":
			list, err := expectList(v, "defaults.mcp_servers", path)
			if err != nil {
				return err
			}
			for i, item := range list {
				base := fmt.Sprintf("defaults.mcp_servers[%d]", i)
				pn, err := expectMapping(item, base, path)
				if err != nil {
					return err
				}
				entry, err := parseMcpEntry(pn, base, path)
				if err != nil {
					return err
				}
				defs.McpServers = append(defs.McpServers, entry)
			}
		default:
			return fmt.Errorf("%s: defaults: unknown key %q (known: compose, mcp_servers, plugins)", path, key)
		}
	}
	return nil
}

// mcpEntryKeys mirrors the frozen image contract's mcp_servers entry
// key set (container/spec.py _MCP_ENTRY_KEYS); values pass through raw.
var mcpEntryKeys = map[string]bool{
	"name": true, "command": true, "url": true, "args": true, "env": true,
	"headers": true, "no_probe": true, "timeout": true, "if_env": true,
	"config": true, "auth": true, "oauth": true, "transport": true,
}

func parseMcpEntry(node map[string]any, base, path string) (McpEntry, error) {
	for key := range node {
		if !mcpEntryKeys[key] {
			return McpEntry{}, fmt.Errorf("%s: %s: unknown key %q (the spec mcp_servers key set applies)", path, base, key)
		}
	}
	name, _ := node["name"].(string)
	if name == "" {
		return McpEntry{}, fmt.Errorf("%s: %s: name is required and must be a non-empty string", path, base)
	}
	_, hasCommand := node["command"]
	_, hasURL := node["url"]
	switch {
	case hasCommand && hasURL:
		return McpEntry{}, fmt.Errorf("%s: %s: stdio and remote shapes are mutually exclusive — set exactly one of command or url", path, base)
	case !hasCommand && !hasURL:
		return McpEntry{}, fmt.Errorf("%s: %s: set exactly one of command (stdio) or url (HTTP)", path, base)
	}
	return McpEntry{Name: name, Raw: node}, nil
}

func applyServices(m *Manifest, val any, path string) error {
	node, err := expectMapping(val, "services", path)
	if err != nil {
		return err
	}
	for name, v := range node {
		if nameRE.MatchString(name) == false {
			return fmt.Errorf("%s: services: invalid name %q (lowercase letters, digits, - _)", path, name)
		}
		base := "services." + name
		entry := ServiceEntry{Name: name, Dir: filepath.Join(m.Root, ServicesDir, name)}
		sn, err := expectMapping(v, base, path)
		if err != nil {
			return err
		}
		for key, sv := range sn {
			switch key {
			case "dir":
				s, err := expectString(sv, base+".dir", path)
				if err != nil {
					return err
				}
				if err := safeRelDir(s); err != nil {
					return fmt.Errorf("%s: %s.dir: %w", path, base, err)
				}
				entry.Dir = filepath.Join(m.Root, s)
			case "networks":
				list, err := expectList(sv, base+".networks", path)
				if err != nil {
					return err
				}
				for i, item := range list {
					s, ok := item.(string)
					if !ok || s == "" {
						return fmt.Errorf("%s: %s.networks[%d]: want a non-empty string", path, base, i)
					}
					entry.Networks = append(entry.Networks, s)
				}
			default:
				return fmt.Errorf("%s: %s: unknown key %q (known: dir, networks)", path, base, key)
			}
		}
		m.Services[name] = entry
	}
	return nil
}

func applyAgents(m *Manifest, val any, path string) error {
	node, err := expectMapping(val, "agents", path)
	if err != nil {
		return err
	}
	for name, v := range node {
		if nameRE.MatchString(name) == false {
			return fmt.Errorf("%s: agents: invalid name %q (lowercase letters, digits, - _ — it is also the directory and compose project name)", path, name)
		}
		base := "agents." + name
		entry := AgentEntry{Name: name, Dir: filepath.Join(m.Root, AgentsDir, name)}
		var an map[string]any
		if v == nil {
			an = map[string]any{} // a bare `grow:` entry is the null-authored empty mapping
		} else {
			var err error
			an, err = expectMapping(v, base, path)
			if err != nil {
				return err
			}
		}
		for key, av := range an {
			switch key {
			case "platform":
				s, err := expectString(av, base+".platform", path)
				if err != nil {
					return err
				}
				entry.Platform = s
			case "gateway_port":
				n, err := expectPort(av, base+".gateway_port", path)
				if err != nil {
					return err
				}
				entry.GatewayPort = n
				entry.ExplicitPort = true
			case "litellm":
				s, err := expectString(av, base+".litellm", path)
				if err != nil {
					return err
				}
				if s != LiteLLMShared && s != LiteLLMSidecar {
					return fmt.Errorf("%s: %s.litellm: want %q or %q, got %q", path, base, LiteLLMShared, LiteLLMSidecar, s)
				}
				entry.LiteLLM = s
			case "fly":
				fn, err := expectMapping(av, base+".fly", path)
				if err != nil {
					return err
				}
				for fk, fv := range fn {
					switch fk {
					case "app":
						s, err := expectString(fv, base+".fly.app", path)
						if err != nil {
							return err
						}
						entry.FlyApp = s
					case "region":
						s, err := expectString(fv, base+".fly.region", path)
						if err != nil {
							return err
						}
						entry.FlyRegion = s
					default:
						return fmt.Errorf("%s: %s.fly: unknown key %q (known: app, region)", path, base, fk)
					}
				}
				if entry.FlyApp == "" {
					return fmt.Errorf("%s: %s.fly.app is required when a fly block is present", path, base)
				}
			case "compose_file":
				s, err := expectString(av, base+".compose_file", path)
				if err != nil {
					return err
				}
				if err := safeRelDir(s); err != nil {
					return fmt.Errorf("%s: %s.compose_file: %w", path, base, err)
				}
				entry.ComposeFile = s
			case "overrides":
				on, err := expectMapping(av, base+".overrides", path)
				if err != nil {
					return err
				}
				ov := &Overrides{}
				for ok, ovv := range on {
					switch ok {
					case "volumes", "ports":
						list, err := expectList(ovv, base+".overrides."+ok, path)
						if err != nil {
							return err
						}
						for i, item := range list {
							s, isStr := item.(string)
							if !isStr || s == "" {
								return fmt.Errorf("%s: %s.overrides.%s[%d]: want a non-empty string", path, base, ok, i)
							}
							if ok == "volumes" {
								ov.Volumes = append(ov.Volumes, s)
							} else {
								ov.Ports = append(ov.Ports, s)
							}
						}
					case "limits":
						ln, err := expectMapping(ovv, base+".overrides.limits", path)
						if err != nil {
							return err
						}
						limits := &ResourceLimits{Cpus: "2.0", Memory: "2g", Pids: 512}
						for lk, lv := range ln {
							switch lk {
							case "cpus":
								s, err := expectString(lv, base+".overrides.limits.cpus", path)
								if err != nil {
									return err
								}
								limits.Cpus = s
							case "memory":
								s, err := expectString(lv, base+".overrides.limits.memory", path)
								if err != nil {
									return err
								}
								limits.Memory = s
							case "pids":
								n, ok := lv.(int)
								if !ok || n < 1 {
									return fmt.Errorf("%s: %s.overrides.limits.pids: want a positive integer", path, base)
								}
								limits.Pids = n
							default:
								return fmt.Errorf("%s: %s.overrides.limits: unknown key %q (known: cpus, memory, pids)", path, base, lk)
							}
						}
						ov.Limits = limits
					default:
						return fmt.Errorf("%s: %s.overrides: unknown key %q (known: limits, ports, volumes)", path, base, ok)
					}
				}
				entry.Overrides = ov
			default:
				return fmt.Errorf("%s: %s: unknown key %q (known: compose_file, fly, gateway_port, litellm, overrides, platform)", path, base, key)
			}
		}
		m.Agents[name] = entry
	}
	return nil
}

// allocatePorts resolves GatewayPort for agents without an explicit
// port: sorted name order (deterministic regardless of YAML document
// order), stepping past ports already taken. Duplicate explicit ports
// are a hard error.
func (m *Manifest) allocatePorts() error {
	taken := map[int]string{}
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		if !entry.ExplicitPort {
			continue
		}
		if other, clash := taken[entry.GatewayPort]; clash {
			return fmt.Errorf("%s: agents.%s.gateway_port: %d is already assigned to agent %q — ports must be unique", m.Path, name, entry.GatewayPort, other)
		}
		taken[entry.GatewayPort] = name
	}
	candidate := m.Plane.GatewayBasePort
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		if entry.ExplicitPort {
			continue
		}
		for {
			if _, clash := taken[candidate]; clash {
				candidate++
				continue
			}
			break
		}
		entry.GatewayPort = candidate
		taken[candidate] = name
		m.Agents[name] = entry
	}
	return nil
}

// resolveLiteLLM fills LiteLLMUsed. An unset value inherits the plane:
// shared when the plane provides a shared proxy, sidecar otherwise; an
// explicit value is honored (shared still requires the plane).
func (m *Manifest) resolveLiteLLM() error {
	planeShared := m.Plane.Enabled && m.Plane.LiteLLM == LiteLLMShared
	for _, name := range m.AgentNames() {
		entry := m.Agents[name]
		switch entry.LiteLLM {
		case LiteLLMShared:
			if !planeShared {
				return fmt.Errorf("%s: agents.%s.litellm: shared requires an enabled plane with plane.litellm: %s", m.Path, name, LiteLLMShared)
			}
			entry.LiteLLMUsed = LiteLLMShared
		case LiteLLMSidecar:
			entry.LiteLLMUsed = LiteLLMSidecar
		case "":
			if planeShared {
				entry.LiteLLMUsed = LiteLLMShared
			} else {
				entry.LiteLLMUsed = LiteLLMSidecar
			}
		}
		m.Agents[name] = entry
	}
	return nil
}

// safeRelDir rejects absolute paths and parent traversal.
func safeRelDir(dir string) error {
	if filepath.IsAbs(dir) {
		return fmt.Errorf("%q must be relative to the fleet root", dir)
	}
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == ".." {
			return fmt.Errorf("%q must not traverse outside the fleet root", dir)
		}
	}
	return nil
}

// stringifyMaps rewrites yaml's map[any]any fallback (triggered by
// non-string mapping keys, e.g. an unquoted numeric agent name like
// `001:`) into map[string]any, failing closed on genuinely non-string
// keys with the offending key named.
func stringifyMaps(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			n, err := stringifyMaps(val)
			if err != nil {
				return nil, err
			}
			t[k] = n
		}
		return t, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			s, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("mapping key %v is not a string — quote names that look numeric", k)
			}
			n, err := stringifyMaps(val)
			if err != nil {
				return nil, err
			}
			out[s] = n
		}
		return out, nil
	default:
		return v, nil
	}
}

func expectMapping(val any, where, path string) (map[string]any, error) {
	m, ok := val.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s: want a mapping, got %T", path, where, val)
	}
	return m, nil
}

func expectList(val any, where, path string) ([]any, error) {
	l, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s: want a list, got %T", path, where, val)
	}
	return l, nil
}

func expectString(val any, where, path string) (string, error) {
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("%s: %s: want a string, got %T", path, where, val)
	}
	if s == "" {
		return "", fmt.Errorf("%s: %s: want a non-empty string", path, where)
	}
	return s, nil
}

func expectBool(val any, where, path string) (bool, error) {
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("%s: %s: want a boolean, got %T", path, where, val)
	}
	return b, nil
}

func expectPort(val any, where, path string) (int, error) {
	n, ok := val.(int)
	if !ok {
		return 0, fmt.Errorf("%s: %s: want an integer port, got %T", path, where, val)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%s: %s: port %d out of range (1-65535)", path, where, n)
	}
	return n, nil
}
