// registry.go is agentctl's composition root for deployment platforms:
// the explicit name→adapter table behind `platform ls` / `platform set`
// and the For resolver the release verbs dispatch through. It lives in
// cli, not internal/platform, because adapters import the port — the
// port package must not import its own adapters. No plugin loading, no
// reflection; adding a platform means an import and a line here.
package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/platform/dockercompose"
	"github.com/tankdonut/agent-base/internal/platform/fly"
	"github.com/tankdonut/agent-base/internal/process"
)

// platformInfo describes one registered platform for `platform ls`.
type platformInfo struct {
	name        string
	description string
	// defaultPlatform marks the platform assumed when .agentctl.yaml
	// omits `platform:` — compose, the reference adapter.
	defaultPlatform bool
}

// platformFactory builds an adapter. ns is the adapter's .agentctl.yaml
// namespace decoded verbatim (nil when absent — factories apply their
// defaults); each adapter decodes its own typed config from it and
// fails closed on unknown keys.
type platformFactory func(r process.Runner, ns map[string]any) (platform.Platform, error)

var platformRegistry = map[string]struct {
	info    platformInfo
	factory platformFactory
}{
	"compose": {
		info: platformInfo{
			name:            "compose",
			description:     "local docker/podman compose (the reference adapter)",
			defaultPlatform: true,
		},
		factory: dockercompose.New,
	},
	"fly": {
		info: platformInfo{
			name:        "fly",
			description: "fly.io — single machine + volume, remote build via flyctl",
		},
		factory: fly.New,
	},
}

// platformNames returns the registered platform names, sorted.
func platformNames() []string {
	out := make([]string, 0, len(platformRegistry))
	for name := range platformRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// platformInfos returns the registry descriptors, sorted by name.
func platformInfos() []platformInfo {
	names := platformNames()
	out := make([]platformInfo, 0, len(names))
	for _, name := range names {
		out = append(out, platformRegistry[name].info)
	}
	return out
}

// forPlatform resolves name to a constructed adapter. namespaces maps
// each platform name to its config namespace; factories read only their
// own. An unknown name fails closed listing what exists.
func forPlatform(name string, r process.Runner, namespaces map[string]map[string]any) (platform.Platform, error) {
	e, ok := platformRegistry[name]
	if !ok {
		return nil, fmt.Errorf("unknown platform %q (available: %s)", name, strings.Join(platformNames(), ", "))
	}
	return e.factory(r, namespaces[name])
}
