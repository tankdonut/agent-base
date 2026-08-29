package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
)

// WorktreeCreate adds a git worktree for branch under .worktrees/, then
// symlinks its agent/.env to the main checkout's copy (relative target,
// so the tree survives moves) — one secrets file per repo, no copies.
func WorktreeCreate(r Runner, root, branch string) error {
	if r == nil {
		return errNilRunner
	}
	worktree := filepath.Join(".worktrees", branch)
	exists := runArgv(r, nil, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch) == nil
	var err error
	if exists {
		err = runArgv(r, nil, "git", "worktree", "add", worktree, branch)
	} else {
		err = runArgv(r, nil, "git", "worktree", "add", "-b", branch, worktree)
	}
	if err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}

	link := filepath.Join(root, ".worktrees", branch, "agent", ".env")
	// Real git already created the tree; MkdirAll also covers fake
	// runners in tests.
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink — refusing to overwrite", link)
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	target, err := filepath.Rel(filepath.Dir(link), filepath.Join(root, "agent", ".env"))
	if err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// WorktreeRemove deletes the worktree's agent/.env symlink if present,
// then removes the worktree via git.
func WorktreeRemove(r Runner, root, branch string) error {
	if r == nil {
		return errNilRunner
	}
	link := filepath.Join(root, ".worktrees", branch, "agent", ".env")
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return runArgv(r, nil, "git", "worktree", "remove", filepath.Join(".worktrees", branch))
}
