package lifecycle

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
)

// ResolveGatewayPort prefers AGENT_GATEWAY_PORT from agent/.env (the
// host-side compose interpolation var) and falls back to the configured
// default (viper gateway_port) when unset or unparseable.
func ResolveGatewayPort(root string, fallback int) int {
	data, err := readFileIfExists(filepath.Join(root, "agent", ".env"))
	if err != nil {
		return fallback
	}
	if v := parseEnvValues(data)["AGENT_GATEWAY_PORT"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// Open prints the gateway URL and opens it with xdg-open when present
// (otherwise printing is the whole success — exit 0).
func Open(r Runner, root string, fallbackPort int, stdout io.Writer) error {
	if r == nil {
		return errNilRunner
	}
	port := ResolveGatewayPort(root, fallbackPort)
	url := fmt.Sprintf("http://localhost:%d", port)
	fmt.Fprintln(stdout, url)
	if _, err := lookPath(r, "xdg-open"); err != nil {
		return nil
	}
	return runArgv(r, nil, "xdg-open", url)
}
