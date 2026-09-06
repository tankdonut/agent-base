package project

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseTagFromDockerfile(t *testing.T) {
	tests := []struct {
		name    string
		docker  string
		want    string
		wantErr string
	}{
		{"plain date tag", "FROM ghcr.io/tankdonut/agent-base:2026.08.28\n", "2026.08.28", ""},
		{"same-day run suffix", "FROM ghcr.io/tankdonut/agent-base:2026.08.24.3\n", "2026.08.24.3", ""},
		{"digest suffix carried", "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc123\n", "2026.08.28@sha256:abc123", ""},
		{"multi-stage alias dropped", "FROM ghcr.io/tankdonut/agent-base:2026.08.28 AS base\n", "2026.08.28", ""},
		{"digest and alias", "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc AS base\n", "2026.08.28@sha256:abc", ""},
		{"indented line", "  FROM ghcr.io/tankdonut/agent-base:2026.08.27\n", "2026.08.27", ""},
		{"no base line", "FROM debian:bookworm\n", "", "no `FROM ghcr.io/tankdonut/agent-base:<tag>` line"},
		{"empty tag", "FROM ghcr.io/tankdonut/agent-base:\n", "", "empty base image tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProject(t, map[string]string{"agent/Dockerfile": tt.docker})
			got, err := BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("tag = %q, want %q", got, tt.want)
			}
		})
	}
}
