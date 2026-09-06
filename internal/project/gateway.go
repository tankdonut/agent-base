package project

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
)

// ResolveGatewayPort prefers AGENT_GATEWAY_PORT from agent/.env (the
// host-side compose interpolation var) and falls back to the configured
// default when unset or unparseable.
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

// WarnGatewayPortBusy probes the loopback bind an upcoming compose up
// will publish. Busy is a warning, never an error: the port may be held
// by this stack's own running container (idempotent re-up) or by another
// agent on the host defaulting to the same port — the warning names the
// fix for the second case.
func WarnGatewayPortBusy(w io.Writer, root string, fallbackPort int) {
	addr := fmt.Sprintf("127.0.0.1:%d", ResolveGatewayPort(root, fallbackPort))
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		_ = listener.Close()
		return
	}
	fmt.Fprintf(w,
		"warning: %s is already bound — if another agent or service on this host holds it, set a distinct AGENT_GATEWAY_PORT in agent/.env; if this stack is already up, ignore this warning\n",
		addr,
	)
}
