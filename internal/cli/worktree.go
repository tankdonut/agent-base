package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/process"
)

// validateBranch rejects branch names that would escape .worktrees/ or
// inject options into the git argv ("../x", absolute paths, leading "-").
func validateBranch(branch string) error {
	switch {
	case branch == "":
		return fmt.Errorf("branch name is empty")
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("branch %q must not start with '-' — git would parse it as an option", branch)
	case filepath.IsAbs(branch) || strings.Contains(branch, ".."):
		return fmt.Errorf("branch %q must be a plain branch name under .worktrees/", branch)
	}
	return nil
}

// WorktreeCreate adds a git worktree for branch under .worktrees/, then
// symlinks its agent/.env to the main checkout's copy (relative target,
// so the tree survives moves) — one secrets file per repo, no copies.
func WorktreeCreate(r process.Runner, root, branch string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	worktree := filepath.Join(".worktrees", branch)
	exists := process.RunArgv(r, nil, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch) == nil
	var err error
	if exists {
		err = process.RunArgv(r, nil, "git", "worktree", "add", worktree, branch)
	} else {
		err = process.RunArgv(r, nil, "git", "worktree", "add", "-b", branch, worktree)
	}
	if err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	if err := linkWorktreeEnv(root, branch); err != nil {
		return fmt.Errorf("%w — worktree left at %s: clean up with `git worktree remove %s` and retry", err, worktree, worktree)
	}
	return nil
}

// linkWorktreeEnv symlinks .worktrees/<branch>/agent/.env to the main
// checkout's agent/.env.
func linkWorktreeEnv(root, branch string) error {
	link := filepath.Join(root, ".worktrees", branch, "agent", ".env")
	// Real git already created the tree; MkdirAll also covers fake
	// runners in tests.
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(link), err)
	}
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink — refusing to overwrite", link)
		}
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("removing stale symlink %s: %w", link, err)
		}
	}
	target, err := filepath.Rel(filepath.Dir(link), filepath.Join(root, "agent", ".env"))
	if err != nil {
		return fmt.Errorf("computing relative target: %w", err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("symlinking %s: %w", link, err)
	}
	return nil
}

// WorktreeRemove deletes the worktree's agent/.env symlink if present,
// then removes the worktree via git.
func WorktreeRemove(r process.Runner, root, branch string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	link := filepath.Join(root, ".worktrees", branch, "agent", ".env")
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("removing %s: %w", link, err)
		}
	}
	return process.RunArgv(r, nil, "git", "worktree", "remove", filepath.Join(".worktrees", branch))
}

// newWorktreeCmd wires the worktree commands.
func newWorktreeCmd() *cobra.Command {
	worktree := &cobra.Command{
		Use:   "worktree",
		Short: "Manage git worktrees sharing the main agent/.env",
	}
	var create = &cobra.Command{
		Use:   "create <branch>",
		Short: "Add .worktrees/<branch> with agent/.env symlinked to the main copy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			return WorktreeCreate(newRunner(), root, args[0])
		},
	}
	var remove = &cobra.Command{
		Use:   "remove <branch>",
		Short: "Remove the <branch> worktree (and its .env symlink)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := chdirProject()
			if err != nil {
				return err
			}
			return WorktreeRemove(newRunner(), root, args[0])
		},
	}
	worktree.AddCommand(create, remove)
	return worktree
}
