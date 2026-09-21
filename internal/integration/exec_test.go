//go:build integration

package integration

import (
	"context"
	"os/exec"
)

// commandIn builds a contained-context command with cwd. stdio is
// left unset so CombinedOutput works — pre-setting Stdout makes
// CombinedOutput fail with "Stdout already set" (empty dumps).
func commandIn(ctx context.Context, dir string, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd
}
