package project

import (
	"os"
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
		{"staging repo tag", "FROM ghcr.io/tankdonut/agent-base-staging:2026.09.22\n", "2026.09.22", ""},
		{"staging repo digest only", "FROM ghcr.io/tankdonut/agent-base-staging@sha256:abc123\n", "sha256:abc123", ""},
		{"digest suffix carried", "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc123\n", "2026.08.28@sha256:abc123", ""},
		{"multi-stage alias dropped", "FROM ghcr.io/tankdonut/agent-base:2026.08.28 AS base\n", "2026.08.28", ""},
		{"digest and alias", "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc AS base\n", "2026.08.28@sha256:abc", ""},
		{"indented line", "  FROM ghcr.io/tankdonut/agent-base:2026.08.27\n", "2026.08.27", ""},
		{"foreign repo rejected", "FROM ghcr.io/other/agent-base:2026.08.28\n", "", "no `FROM ghcr.io/tankdonut/agent-base[-staging]:<tag>` line"},
		{"no base line", "FROM debian:bookworm\n", "", "no `FROM ghcr.io/tankdonut/agent-base[-staging]:<tag>` line"},
		{"empty tag", "FROM ghcr.io/tankdonut/agent-base:\n", "", "empty base image ref"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProject(t, map[string]string{"agent/Dockerfile": tt.docker})
			got, err := BaseTagFromDockerfile(filepath.Join(root, "Dockerfile"))
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

// TestRewriteBaseTag pins the upgrade rewrite: round-trip with
// BaseTagFromDockerfile, byte-preservation of everything else on the
// line (indent, AS alias), digest drop (a rewritten pin is tag-only),
// and fail-closed on absent or ambiguous base lines.
func TestRewriteBaseTag(t *testing.T) {
	tests := []struct {
		name    string
		docker  string
		from    string
		to      string
		want    string
		wantErr string
	}{
		{
			name:   "plain rewrite round-trips",
			docker: "FROM ghcr.io/tankdonut/agent-base:2026.08.28\nCOPY agent/ /opt/agent/\n",
			from:   "2026.08.28",
			to:     "2026.09.12.1",
			want:   "FROM ghcr.io/tankdonut/agent-base:2026.09.12.1\nCOPY agent/ /opt/agent/\n",
		},
		{
			name:   "indent and alias preserved",
			docker: "  FROM ghcr.io/tankdonut/agent-base:2026.08.28 AS base\nRUN true\n",
			from:   "2026.08.28",
			to:     "2026.09.12",
			want:   "  FROM ghcr.io/tankdonut/agent-base:2026.09.12 AS base\nRUN true\n",
		},
		{
			name:   "digest pin drops to tag-only",
			docker: "FROM ghcr.io/tankdonut/agent-base:2026.08.28@sha256:abc\n",
			from:   "2026.08.28@sha256:abc",
			to:     "2026.09.12",
			want:   "FROM ghcr.io/tankdonut/agent-base:2026.09.12\n",
		},
		{
			name:    "no base line",
			docker:  "FROM debian:bookworm\n",
			from:    "2026.08.28",
			to:      "2026.09.12",
			wantErr: "no `FROM ghcr.io/tankdonut/agent-base[-staging]:<tag>` line",
		},
		{
			name:    "ambiguous base lines",
			docker:  "FROM ghcr.io/tankdonut/agent-base:2026.08.28\nFROM ghcr.io/tankdonut/agent-base:2026.09.05\n",
			from:    "2026.08.28",
			to:      "2026.09.12",
			wantErr: "2 base image FROM lines",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(writeProject(t, map[string]string{"agent/Dockerfile": tt.docker}), "Dockerfile")
			err := RewriteBaseTag(path, tt.to)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.want {
				t.Errorf("rewritten Dockerfile =\n%q\nwant\n%q", data, tt.want)
			}
			if got, err := BaseTagFromDockerfile(path); err != nil || got != tt.to {
				t.Errorf("round-trip: BaseTagFromDockerfile = %q, %v; want %q", got, err, tt.to)
			}
		})
	}
}
