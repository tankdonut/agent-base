// Command agentctl is the operator CLI for downstream agent projects
// consuming the agent-base container image.
package main

import (
	"os"

	"github.com/tankdonut/agent-base/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
