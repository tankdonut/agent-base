package lifecycle

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenPortResolution(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		envFile  bool
		fallback int
		wantURL  string
	}{
		{"env file beats viper default", "AGENT_GATEWAY_PORT=9999\n", true, 18789, "http://localhost:9999"},
		{"unset falls back to viper", "#AGENT_GATEWAY_PORT=18789\n", true, 3000, "http://localhost:3000"},
		{"no env file falls back to viper", "", false, 4000, "http://localhost:4000"},
		{"invalid value falls back to viper", "AGENT_GATEWAY_PORT=not-a-port\n", true, 5000, "http://localhost:5000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"agent/spec.json": "{}"}
			if tt.envFile {
				files["agent/.env"] = tt.env
			}
			root := writeProject(t, files)

			var out strings.Builder
			r := newFakeRunner("xdg-open")
			if err := Open(r, root, tt.fallback, &out); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(out.String()); got != tt.wantURL {
				t.Errorf("printed %q, want %q", got, tt.wantURL)
			}
			assertCalls(t, r, [][]string{{"xdg-open", tt.wantURL}})
		})
	}
}

func TestOpenWithoutXdgOpen(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json": "{}",
		"agent/.env":      "AGENT_GATEWAY_PORT=9999\n",
	})
	var out strings.Builder
	r := newFakeRunner() // no xdg-open on PATH
	if err := Open(r, root, 18789, &out); err != nil {
		t.Fatalf("absent xdg-open must still succeed: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("no exec expected without xdg-open, got %v", r.calls)
	}
	if got := strings.TrimSpace(out.String()); got != "http://localhost:9999" {
		t.Errorf("printed %q", got)
	}
}

func TestResolveGatewayPortDefault(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}"})
	if got := ResolveGatewayPort(filepath.Join(root), 18789); got != 18789 {
		t.Errorf("port = %d, want 18789", got)
	}
}

func TestWarnGatewayPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("loopback listen unavailable: %v", err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}()

	tests := []struct {
		name     string
		env      string
		fallback int
		wantAddr string // "" expects silence
	}{
		{"free port stays silent", "", freePort, ""},
		{"busy fallback port warns", "", busyPort, fmt.Sprintf("127.0.0.1:%d", busyPort)},
		{"env override reaches busy port",
			fmt.Sprintf("AGENT_GATEWAY_PORT=%d\n", busyPort), freePort,
			fmt.Sprintf("127.0.0.1:%d", busyPort)},
		{"env override to a free port stays silent",
			fmt.Sprintf("AGENT_GATEWAY_PORT=%d\n", freePort), busyPort, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"agent/spec.json": "{}"}
			if tt.env != "" {
				files["agent/.env"] = tt.env
			}
			root := writeProject(t, files)
			var out strings.Builder
			WarnGatewayPortBusy(&out, root, tt.fallback)
			got := out.String()
			if tt.wantAddr == "" {
				if got != "" {
					t.Errorf("unexpected warning: %q", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantAddr) || !strings.Contains(got, "AGENT_GATEWAY_PORT") {
				t.Errorf("warning = %q, want addr %q plus the AGENT_GATEWAY_PORT fix", got, tt.wantAddr)
			}
		})
	}
}
