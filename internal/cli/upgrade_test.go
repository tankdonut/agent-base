package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tankdonut/agent-base/internal/project"
)

// dockerfilePin reads the fixture's current base tag back.
func dockerfilePin(t *testing.T, root string) string {
	t.Helper()
	tag, err := project.BaseTagFromDockerfile(filepath.Join(root, "agent", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

// TestUpgradeDryRun pins the no-side-effects plan: the five steps
// named with old→new tags, the Dockerfile untouched, and no platform
// verbs invoked.
func TestUpgradeDryRun(t *testing.T) {
	root := fixtureProject(t)
	pinFixture(t, root, "2099.12.31")
	r := stubbedRunner(t, "podman")
	out, err := execIn(t, root, "upgrade", "2099.12.31.1", "--dry-run")
	if err != nil {
		t.Fatalf("upgrade --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{"upgrade plan", "gate", "backup", "rewrite", "deploy", "verify", "2099.12.31", "2099.12.31.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan lacks %q:\n%s", want, out)
		}
	}
	if got := dockerfilePin(t, root); got != "2099.12.31" {
		t.Errorf("dry-run rewrote the pin to %s", got)
	}
	for _, call := range r.calls {
		if joined := strings.Join(call, " "); strings.Contains(joined, "backup") || strings.Contains(joined, "up -d") {
			t.Errorf("dry-run invoked a platform verb: %s", joined)
		}
	}
}

// TestUpgradeSameTag aborts without doing anything.
func TestUpgradeSameTag(t *testing.T) {
	root := fixtureProject(t)
	pinFixture(t, root, "2099.12.31")
	stubbedRunner(t, "podman")
	if _, err := execIn(t, root, "upgrade", "2099.12.31"); err == nil {
		t.Fatal("upgrading to the pinned tag must error")
	}
	if got := dockerfilePin(t, root); got != "2099.12.31" {
		t.Errorf("same-tag run rewrote the pin to %s", got)
	}
}

// TestUpgradeGateFailureAborts pins the fail-closed gate: a doctor
// --target FAIL (era crossing on a litellm spec) aborts BEFORE the
// backup and rewrite — the Dockerfile stays pinned and the output
// carries the runbook.
func TestUpgradeGateFailureAborts(t *testing.T) {
	root := litellmFixture(t)
	addLitellmSidecar(t, root)
	pinFixture(t, root, "2026.08.22")
	r := stubbedRunner(t, "podman")
	r.runOutputOK = true
	out, err := execIn(t, root, "upgrade", "2026.09.12", "--yes")
	if err == nil {
		t.Fatal("gate FAIL must abort the upgrade")
	}
	if !strings.Contains(out, "era/litellm-baseurl-seed") {
		t.Errorf("gate output lacks the era FAIL line:\n%s", out)
	}
	if !strings.Contains(out, postUpgradeRollback) {
		t.Errorf("failure output lacks the rollback runbook:\n%s", out)
	}
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("err = %v, want the nothing-was-changed abort", err)
	}
	if got := dockerfilePin(t, root); got != "2026.08.22" {
		t.Errorf("gate failure rewrote the pin to %s", got)
	}
	for _, call := range r.calls {
		if strings.Contains(strings.Join(call, " "), "backup create") {
			t.Errorf("gate failure ran a backup: %s", call)
		}
	}
}

// TestUpgradeGreenPath walks the whole D8 sequence on stubbed
// platform verbs: gate passes (zero era crossings on future-dated
// tags), backup runs, the pin is rewritten, deploy converges, and the
// post-upgrade verify set runs green against the new tag.
func TestUpgradeGreenPath(t *testing.T) {
	root := fixtureProject(t)
	pinFixture(t, root, "2099.12.31")
	r := stubbedRunner(t, "podman")
	r.runOutputOK = true
	r.runOutputs = greenPostUpgradeOutputs("2099.12.31.1")
	out, err := execIn(t, root, "upgrade", "2099.12.31.1", "--yes")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	for _, want := range []string{
		"era", "backup of", "rewrote agent/Dockerfile", "2099.12.31 → 2099.12.31.1",
		"post-upgrade verification green",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade output lacks %q:\n%s", want, out)
		}
	}
	if got := dockerfilePin(t, root); got != "2099.12.31.1" {
		t.Errorf("pin = %s, want 2099.12.31.1", got)
	}
	sawBackup, sawForceDeploy := false, false
	for _, call := range r.calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "backup create --verify"):
			sawBackup = true
		case strings.Contains(joined, "up -d --force-recreate"):
			sawForceDeploy = true
		}
	}
	if !sawBackup || !sawForceDeploy {
		t.Errorf("missing platform verbs (backup=%v force-deploy=%v):\n%v", sawBackup, sawForceDeploy, r.calls)
	}
}

// TestUpgradeVerifyFailureNamesRunbook pins the tail: when
// post-upgrade verification FAILs (marker still on the old tag), the
// runbook prints and the verb exits non-zero — after the rewrite,
// because the mutation already happened. The readiness wait is
// collapsed (the stub's status.json never records the target).
func TestUpgradeVerifyFailureNamesRunbook(t *testing.T) {
	oldTimeout := upgradeWaitTimeout
	upgradeWaitTimeout = 10 * time.Millisecond
	t.Cleanup(func() { upgradeWaitTimeout = oldTimeout })
	root := fixtureProject(t)
	pinFixture(t, root, "2099.12.31")
	r := stubbedRunner(t, "podman")
	r.runOutputOK = true
	r.runOutputs = greenPostUpgradeOutputs("2099.12.31") // marker stuck on the old tag
	out, err := execIn(t, root, "upgrade", "2099.12.31.1", "--yes")
	if err == nil {
		t.Fatal("verify FAIL must exit non-zero")
	}
	if !strings.Contains(out, "running image is 2099.12.31, expected 2099.12.31.1") {
		t.Errorf("verify failure lacks the marker mismatch line:\n%s", out)
	}
	if !strings.Contains(out, postUpgradeRollback) {
		t.Errorf("verify failure lacks the rollback runbook:\n%s", out)
	}
	if got := dockerfilePin(t, root); got != "2099.12.31.1" {
		t.Errorf("pin = %s — the rewrite must persist for the runbook's revert step", got)
	}
}

// TestUpgradeConfirmPrompt pins the interactive gate: the operator
// types the NEW tag to proceed; anything else aborts untouched.
func TestUpgradeConfirmPrompt(t *testing.T) {
	root := fixtureProject(t)
	pinFixture(t, root, "2099.12.31")
	r := stubbedRunner(t, "podman")
	r.runOutputOK = true
	r.runOutputs = greenPostUpgradeOutputs("2099.12.31.1")
	out, err := execInStdin(t, root, "nope\n", "upgrade", "2099.12.31.1")
	if err == nil || !strings.Contains(err.Error(), "aborted — nothing was changed") {
		t.Fatalf("wrong confirmation must abort naming it: %v\n%s", err, out)
	}
	if got := dockerfilePin(t, root); got != "2099.12.31" {
		t.Errorf("aborted run rewrote the pin to %s", got)
	}
	out, err = execInStdin(t, root, "2099.12.31.1\n", "upgrade", "2099.12.31.1")
	if err != nil {
		t.Fatalf("correct confirmation must proceed: %v\n%s", err, out)
	}
	if got := dockerfilePin(t, root); got != "2099.12.31.1" {
		t.Errorf("confirmed run left the pin at %s", got)
	}
}

// TestUpgradeMalformedTarget fails closed on the tag shape.
func TestUpgradeMalformedTarget(t *testing.T) {
	root := fixtureProject(t)
	stubbedRunner(t, "podman")
	if _, err := execIn(t, root, "upgrade", "latest"); err == nil {
		t.Fatal("malformed target must error")
	}
}
