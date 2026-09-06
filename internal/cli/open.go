package cli

import (
	"fmt"
	"io"

	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// Open prints the gateway URL and opens it with xdg-open when present
// (otherwise printing is the whole success — exit 0).
func Open(r process.Runner, root string, fallbackPort int, stdout io.Writer) error {
	if r == nil {
		return process.ErrNilRunner
	}
	port := project.ResolveGatewayPort(root, fallbackPort)
	url := fmt.Sprintf("http://localhost:%d", port)
	fmt.Fprintln(stdout, url)
	if _, err := process.LookPath(r, "xdg-open"); err != nil {
		return nil
	}
	return process.RunArgv(r, nil, "xdg-open", url)
}
