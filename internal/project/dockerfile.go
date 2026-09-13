package project

import (
	"fmt"
	"os"
	"strings"
)

// baseImagePrefix identifies the project Dockerfile's base image line.
const baseImagePrefix = "FROM ghcr.io/tankdonut/agent-base:"

// BaseTagFromDockerfile extracts the base image tag from the project
// Dockerfile: the first whitespace-delimited field after the colon on
// the `FROM ghcr.io/tankdonut/agent-base:<tag>` line. That keeps a
// possible @sha256 digest suffix (no spaces) while dropping multi-stage
// aliases (`...:<tag> AS base`).
func BaseTagFromDockerfile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, baseImagePrefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(trimmed, baseImagePrefix))
		if len(fields) == 0 {
			return "", fmt.Errorf("%s: empty base image tag", path)
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("%s: no `FROM ghcr.io/tankdonut/agent-base:<tag>` line found", path)
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
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), baseImagePrefix) {
			if found >= 0 {
				return fmt.Errorf("%s: %d base image FROM lines — exactly one expected; fix the Dockerfile first", path, i+1)
			}
			found = i
		}
	}
	if found < 0 {
		return fmt.Errorf("%s: no `FROM ghcr.io/tankdonut/agent-base:<tag>` line found", path)
	}
	line := lines[found]
	trimmed := strings.TrimSpace(line)
	rest := strings.TrimPrefix(trimmed, baseImagePrefix)
	ref := rest
	if fields := strings.Fields(rest); len(fields) > 0 {
		ref = fields[0]
	}
	if ref == "" {
		return fmt.Errorf("%s: empty base image tag", path)
	}
	lines[found] = strings.Replace(line, baseImagePrefix+ref, baseImagePrefix+newTag, 1)
	out := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
