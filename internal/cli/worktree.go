package cli

import (
	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/lifecycle"
)

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
			return lifecycle.WorktreeCreate(newRunner(), root, args[0])
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
			return lifecycle.WorktreeRemove(newRunner(), root, args[0])
		},
	}
	worktree.AddCommand(create, remove)
	return worktree
}
