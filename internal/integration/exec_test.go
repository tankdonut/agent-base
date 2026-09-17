//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
)

// commandIn builds a contained-context command with cwd + inherited stdio.
func commandIn(ctx context.Context, dir string, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}
