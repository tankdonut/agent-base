package lifecycle

import (
	"strings"
	"testing"
)

func TestResolveEngine(t *testing.T) {
	tests := []struct {
		name    string
		pref    string
		look    []string
		want    string
		wantErr string
	}{
		{"auto prefers podman", "auto", []string{"podman", "docker"}, "podman", ""},
		{"auto falls back to docker", "auto", []string{"docker"}, "docker", ""},
		{"empty pref means auto", "", []string{"docker"}, "docker", ""},
		{"explicit podman", "podman", []string{"podman"}, "podman", ""},
		{"explicit docker", "docker", []string{"docker"}, "docker", ""},
		{"explicit override beats podman presence", "docker", []string{"podman", "docker"}, "docker", ""},
		{"auto with neither errors", "auto", nil, "", "install podman or docker"},
		{"explicit missing binary errors", "podman", []string{"docker"}, "", `engine "podman" not found in PATH`},
		{"invalid value errors", "containerd", []string{"podman"}, "", "want auto, podman, or docker"},
		{"nil runner errors", "auto", nil, "", "nil runner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Runner
			if tt.name != "nil runner errors" {
				r = newFakeRunner(tt.look...)
			}
			got, err := ResolveEngine(tt.pref, r)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveEngine(%q) err = %v, want containing %q", tt.pref, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveEngine(%q): %v", tt.pref, err)
			}
			if got != tt.want {
				t.Errorf("ResolveEngine(%q) = %q, want %q", tt.pref, got, tt.want)
			}
		})
	}
}
