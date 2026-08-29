package lifecycle

import (
	"strings"
	"testing"
)

func TestPreCommitCheckArgv(t *testing.T) {
	r := newFakeRunner("pre-commit")
	if err := PreCommitCheck(r); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r, [][]string{{"pre-commit", "run", "--all-files"}})
}

func TestPreCommitHooksArgv(t *testing.T) {
	r := newFakeRunner("pre-commit")
	if err := PreCommitHooks(r); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r, [][]string{{"pre-commit", "install"}})
}

func TestPreCommitMissing(t *testing.T) {
	r := newFakeRunner()
	if err := PreCommitCheck(r); err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("check: err = %v, want PATH error", err)
	}
	if err := PreCommitHooks(r); err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("hooks: err = %v, want PATH error", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("nothing may exec, got %v", r.calls)
	}
}
