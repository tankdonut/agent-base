package project

import (
	"fmt"
	"os"
	"strings"
)

// baseImagePrefixes are the project Dockerfile's accepted base image
// lines: the public package (the product contract for downstream
// agents — tag form) and the private staging package in tag or
// digest-only form (digest-pinned candidates in CI qualification runs
// build their agent image FROM the exact bytes under test). Most
// specific first. Anything else is a foreign base and fails closed.
var baseImagePrefixes = []string{
	"FROM ghcr.io/tankdonut/agent-base-staging:",
	"FROM ghcr.io/tankdonut/agent-base-staging",
	"FROM ghcr.io/tankdonut/agent-base:",
}

func basePrefixOf(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	for _, p := range baseImagePrefixes {
		if strings.HasPrefix(trimmed, p) {
			return p, true
		}
	}
	return "", false
}

// BaseTagFromDockerfile extracts the base image ref from the project
// Dockerfile: the first whitespace-delimited field after the prefix on
// a base FROM line. That keeps a possible @sha256 digest suffix (no
// spaces) while dropping multi-stage aliases (`...:<tag> AS base`).
func BaseTagFromDockerfile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		prefix, ok := basePrefixOf(line)
		if !ok {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), prefix))
		if len(fields) == 0 {
			return "", fmt.Errorf("%s: empty base image ref", path)
		}
		return strings.TrimPrefix(fields[0], "@"), nil
	}
	return "", fmt.Errorf("%s: no `FROM ghcr.io/tankdonut/agent-base[-staging]:<tag>` line found", path)
}

// RewriteBaseTag rewrites the base image tag in the project Dockerfile
// to newTag. Exactly one base FROM line must exist — absent or multiple
// base lines abort rather than guess; everything else on the line
// (indentation, an AS alias) and every other line is preserved
// byte-for-byte. The old reference is replaced wholesale, so a digest
// pin becomes tag-only (the caller supplies the tag it verified).
func RewriteBaseTag(path, newTag string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	found := -1
	prefix := ""
	for i, line := range lines {
		if p, ok := basePrefixOf(line); ok {
			if found >= 0 {
				return fmt.Errorf("%s: %d base image FROM lines — exactly one expected; fix the Dockerfile first", path, i+1)
			}
			found = i
			prefix = p
		}
	}
	if found < 0 {
		return fmt.Errorf("%s: no `FROM ghcr.io/tankdonut/agent-base[-staging]:<tag>` line found", path)
	}
	line := lines[found]
	trimmed := strings.TrimSpace(line)
	rest := strings.TrimPrefix(trimmed, prefix)
	ref := rest
	if fields := strings.Fields(rest); len(fields) > 0 {
		ref = fields[0]
	}
	if ref == "" {
		return fmt.Errorf("%s: empty base image ref", path)
	}
	lines[found] = strings.Replace(line, prefix+ref, prefix+newTag, 1)
	out := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
