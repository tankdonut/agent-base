package cli

import (
	"strings"
	"testing"
)

func assertCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPreCommitCheckArgv(t *testing.T) {
	r := newStubRunner("pre-commit")
	if err := PreCommitCheck(r); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls, [][]string{{"pre-commit", "run", "--all-files"}})
}

func TestPreCommitHooksArgv(t *testing.T) {
	r := newStubRunner("pre-commit")
	if err := PreCommitHooks(r); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls, [][]string{{"pre-commit", "install"}})
}

func TestPreCommitMissing(t *testing.T) {
	r := newStubRunner()
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

func TestSecretsEdit(t *testing.T) {
	r := newStubRunner("vi", "nvim")
	if err := SecretsEdit(r, "nvim", "/proj/agent/.env"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, r.calls, [][]string{{"nvim", "/proj/agent/.env"}})

	if err := SecretsEdit(r, "emacs", "/proj/agent/.env"); err == nil || !strings.Contains(err.Error(), "$EDITOR") {
		t.Fatalf("missing editor: err = %v, want $EDITOR hint", err)
	}

	if err := SecretsEdit(r, "", "/proj/agent/.env"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls[len(r.calls)-1], " ") != "vi /proj/agent/.env" {
		t.Errorf("empty editor argv = %v, want vi", r.calls[len(r.calls)-1])
	}
}
