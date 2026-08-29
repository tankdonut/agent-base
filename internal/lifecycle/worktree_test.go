package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeCreateNewBranch(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}", "agent/.env": "X=1\n"})
	r := newFakeRunner("git")
	// show-ref fails → branch does not exist yet → create with -b.
	r.failArgv = [][]string{{"git", "show-ref", "--verify", "--quiet", "refs/heads/feat/x"}}

	if err := WorktreeCreate(r, root, "feat/x"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r, [][]string{
		{"git", "show-ref", "--verify", "--quiet", "refs/heads/feat/x"},
		{"git", "worktree", "add", "-b", "feat/x", ".worktrees/feat/x"},
	})

	link := filepath.Join(root, ".worktrees", "feat", "x", "agent", ".env")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("symlink not created: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("agent/.env in worktree is not a symlink")
	}
	// Relative target computed from the symlink's own directory — for a
	// multi-component branch that is four levels up, and it must resolve
	// back to the main checkout's agent/.env.
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	wantTarget, err := filepath.Rel(filepath.Dir(link), filepath.Join(root, "agent", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if target != wantTarget {
		t.Errorf("symlink target = %q, want %q", target, wantTarget)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Join(root, "agent", ".env") {
		t.Errorf("symlink resolves to %q, want %q", resolved, filepath.Join(root, "agent", ".env"))
	}
}

func TestWorktreeCreateExistingBranch(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}", "agent/.env": "X=1\n"})
	r := newFakeRunner("git") // show-ref succeeds → branch exists
	if err := WorktreeCreate(r, root, "main"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r, [][]string{
		{"git", "show-ref", "--verify", "--quiet", "refs/heads/main"},
		{"git", "worktree", "add", ".worktrees/main", "main"},
	})
}

func TestWorktreeCreateRefusesRegularFileAtLink(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json":            "{}",
		".worktrees/main/agent/.env": "REAL FILE\n",
	})
	r := newFakeRunner("git")
	err := WorktreeCreate(r, root, "main")
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("err = %v, want not-a-symlink refusal", err)
	}
	// The worktree was already added, so the error must teach the cleanup.
	if !strings.Contains(err.Error(), "git worktree remove") {
		t.Fatalf("err = %v, want orphan-worktree cleanup hint", err)
	}
}

func TestWorktreeRejectsHostileBranchNames(t *testing.T) {
	tests := []struct{ branch, want string }{
		{"../evil", "plain branch name"},
		{"-exec", "must not start with '-'"},
		{"/abs/branch", "plain branch name"},
		{"", "empty"},
	}
	for _, tt := range tests {
		root := writeProject(t, map[string]string{"agent/spec.json": "{}", "agent/.env": "X=1\n"})
		r := newFakeRunner("git")
		err := WorktreeCreate(r, root, tt.branch)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("create(%q) err = %v, want %q", tt.branch, err, tt.want)
		}
		if len(r.calls) != 0 {
			t.Errorf("create(%q) ran git before validating: %v", tt.branch, r.calls)
		}
		err = WorktreeRemove(r, root, tt.branch)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("remove(%q) err = %v, want %q", tt.branch, err, tt.want)
		}
		if len(r.calls) != 0 {
			t.Errorf("remove(%q) ran git before validating: %v", tt.branch, r.calls)
		}
	}
}

func TestWorktreeRemove(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}", "agent/.env": "X=1\n"})
	link := filepath.Join(root, ".worktrees", "feat", "agent", ".env")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", "agent", ".env"), link); err != nil {
		t.Fatal(err)
	}

	r := newFakeRunner("git")
	if err := WorktreeRemove(r, root, "feat"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("symlink still present after remove: %v", err)
	}
	assertCalls(t, r, [][]string{{"git", "worktree", "remove", ".worktrees/feat"}})
}

func TestWorktreeRemoveKeepsRegularFile(t *testing.T) {
	root := writeProject(t, map[string]string{
		"agent/spec.json":            "{}",
		".worktrees/feat/agent/.env": "REAL FILE\n",
	})
	r := newFakeRunner("git")
	if err := WorktreeRemove(r, root, "feat"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".worktrees", "feat", "agent", ".env"))
	if err != nil || string(data) != "REAL FILE\n" {
		t.Errorf("regular file must be left alone: %v %q", err, data)
	}
	assertCalls(t, r, [][]string{{"git", "worktree", "remove", ".worktrees/feat"}})
}
