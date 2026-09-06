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
