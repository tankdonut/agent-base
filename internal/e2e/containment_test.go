package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCompletesAndIsSilent(t *testing.T) {
	root := t.TempDir()
	err := Run(context.Background(), Step{
		Name: "ok", Argv: []string{"true"}, ArtifactRoot: root,
	})
	if err != nil {
		t.Fatalf("true failed: %v", err)
	}
	// A clean run still leaves its stderr.log (empty) — the trail is
	// always inspectable.
	if _, err := os.Stat(filepath.Join(root, "ok-2026")); err == nil {
		// (directories are stamped; existence of ANY ok-* dir is fine)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "ok-*"))
	if len(matches) != 1 {
		t.Errorf("expected exactly one artifact dir, got %v", matches)
	}
}

func TestBudgetBreachKillsGroupAndDumps(t *testing.T) {
	root := t.TempDir()
	var pgid int
	dumped := false
	err := Run(context.Background(), Step{
		Name:         "wedge",
		Argv:         []string{"sh", "-c", "sleep 5 & sleep 5 & wait"},
		Budget:       time.Second,
		ArtifactRoot: root,
		OnStart:      func(p int) { pgid = p },
		Dumpers: []func(string) error{
			func(dir string) error {
				dumped = true
				return os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("evidence"), 0o644)
			},
		},
	})
	if err == nil {
		t.Fatal("budget breach must fail the step")
	}
	if !strings.Contains(err.Error(), "artifacts:") {
		t.Errorf("error must name the artifact dir: %v", err)
	}
	if !dumped {
		t.Error("dumpers must run on breach")
	}
	// The WHOLE group died: signalling the group must eventually report
	// no survivors. Killed members reparented to a non-reaping PID 1
	// linger as zombies that still answer signal(0) — poll briefly for
	// the reaper before failing.
	groupDead := false
	for i := 0; i < 20; i++ {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			groupDead = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !groupDead {
		t.Fatal("process group survived the SIGKILL")
	}
	// Artifacts landed.
	matches, _ := filepath.Glob(filepath.Join(root, "wedge-*"))
	if len(matches) != 1 {
		t.Fatalf("artifact dirs = %v", matches)
	}
	if _, err := os.Stat(filepath.Join(matches[0], "stderr.log")); err != nil {
		t.Errorf("stderr.log missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(matches[0], "extra.txt")); err != nil {
		t.Errorf("dumper output missing: %v", err)
	}
}

func TestNonZeroExitCarriesTail(t *testing.T) {
	root := t.TempDir()
	err := Run(context.Background(), Step{
		Name:         "boom",
		Argv:         []string{"sh", "-c", "echo pre-failure evidence; exit 3"},
		ArtifactRoot: root,
	})
	if err == nil {
		t.Fatal("exit 3 must fail the step")
	}
	if !strings.Contains(err.Error(), "pre-failure evidence") {
		t.Errorf("error tail lost the child output: %v", err)
	}
}

func TestDumperPanicDoesNotMaskFailure(t *testing.T) {
	root := t.TempDir()
	err := Run(context.Background(), Step{
		Name:         "panic-dumper",
		Argv:         []string{"false"},
		ArtifactRoot: root,
		Dumpers: []func(string) error{
			func(string) error { panic("dumper exploded") },
		},
	})
	if err == nil {
		t.Fatal("the original failure must surface")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "panic-dumper-*"))
	if len(matches) != 1 {
		t.Fatalf("artifact dirs = %v", matches)
	}
	if _, err := os.Stat(filepath.Join(matches[0], "dumper-0-panic.txt")); err != nil {
		t.Errorf("panic evidence missing: %v", err)
	}
}

func TestContextCancelKillsGroup(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	var pgid int
	err := Run(ctx, Step{
		Name:         "cancelled",
		Argv:         []string{"sleep", "30"},
		ArtifactRoot: root,
		OnStart:      func(p int) { pgid = p },
	})
	if err == nil {
		t.Fatal("cancellation must fail the step")
	}
	groupDead := false
	for i := 0; i < 20; i++ {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			groupDead = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !groupDead {
		t.Error("process group survived cancellation")
	}
}
