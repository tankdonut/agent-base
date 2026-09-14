// upgrade.go is the upgrade runbook as a verb (D8): doctor --target
// gate → verified backup → FROM-tag rewrite → deploy → post-upgrade
// verify. Ordering mirrors the image-side invariant — backup before
// any mutation — and every failure exit after the confirm prints the
// rollback runbook.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/project"
)

func newUpgradeCmd() *cobra.Command {
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "upgrade <tag>",
		Short: "Upgrade the base image: gate, backup, retag, deploy, verify",
		Long: `Run the upgrade runbook end to end: the doctor --target gate
(era crossings, backup readiness, the target-image spec gate), a
verified backup inside the running instance, the agent/Dockerfile FROM
rewrite, a deploy onto the new image, and the doctor --post-upgrade
verify set. Any failure after the confirmation prints the rollback
runbook and exits non-zero — rollback itself stays manual (restore the
backup, revert the tag, redeploy).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgrade(cmd, args[0], dryRun, yes)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan (gate, backup, rewrite, deploy, verify) and exit")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func runUpgrade(cmd *cobra.Command, target string, dryRun, yes bool) error {
	if tagDate(target).IsZero() {
		return fmt.Errorf("upgrade target %q is not a valid image tag (want YYYY.MM.DD[.N])", target)
	}
	out := cmd.OutOrStdout()
	root, p, d, err := loadProjectPlatform()
	if err != nil {
		return err
	}
	dockerfile := filepath.Join(root, "Dockerfile")
	old, err := project.BaseTagFromDockerfile(dockerfile)
	if err != nil {
		return err
	}
	if old == target {
		return fmt.Errorf("the pinned tag is already %s — nothing to upgrade", target)
	}

	if dryRun {
		printUpgradePlan(out, d.Project, old, target)
		return nil
	}
	if !yes {
		if err := confirmUpgrade(cmd.InOrStdin(), out, d.Project, old, target); err != nil {
			return err
		}
	}

	// From here on, any failure names the rollback runbook: the
	// operator must always see the way back.
	fail := func(cause error, format string, a ...any) error {
		fmt.Fprintln(out, postUpgradeRollback)
		if cause != nil {
			return fmt.Errorf("%w (%s)", cause, fmt.Sprintf(format, a...))
		}
		return fmt.Errorf(format, a...)
	}

	// Gate: the doctor --target preview must pass clean — its FAILs
	// (era crossings, unready volumes, a failing target spec gate)
	// abort before anything changes.
	var gate bytes.Buffer
	if gerr := runDoctor(&gate, false, target, false, "", ""); gerr != nil {
		fmt.Fprint(out, gate.String())
		return fail(gerr, "upgrade gate failed — nothing was changed")
	}
	fmt.Fprint(out, gate.String())

	if !p.Capabilities().Exec {
		return fail(nil, "platform %q cannot exec into the running instance — take a backup by hand (openclaw backup create --verify --output /backups) and re-run; refusing to upgrade unbacked", p.Name())
	}
	if berr := p.Backup(cmd.Context(), newRunner(), root, &d, cmdOut{out}); berr != nil {
		return fail(berr, "backup failed — the pin was not rewritten")
	}
	if rerr := project.RewriteBaseTag(dockerfile, target); rerr != nil {
		return fail(rerr, "tag rewrite failed — the stack still runs %s", old)
	}
	fmt.Fprintf(out, "rewrote agent/Dockerfile: %s → %s\n", old, target)
	// Force the recreate: podman-compose's plain `up -d` does not
	// recreate a container whose image changed (docker compose v2
	// does), and an upgrade whose old container keeps running is no
	// upgrade — this is the deploy --force rebuild path, volumes kept.
	if derr := p.Deploy(cmd.Context(), newRunner(), root, &d, platform.DeployOptions{Force: true}, cmdOut{out}); derr != nil {
		return fail(derr, "deploy failed — the pin points at %s; revert it to %s to roll back", target, old)
	}

	info, serr := project.ReadSpec(filepath.Join(root, "spec.json"))
	if serr != nil {
		return fail(serr, "post-upgrade verify could not read the spec")
	}
	// The verify set expects a healthy gateway with cron seeded. Deploy
	// returns at container start (not health), a warm volume still
	// carries the previous boot's status.json (so its mere presence
	// proves nothing), and the marker is only written once the boot's
	// verified backup completes. Wait for healthz, then for status.json
	// recording the target image — both bounded; a timeout warns and
	// the checks still report the honest state.
	if werr := waitUpgradeReady(cmd.Context(), p, root, &d, target); werr != nil {
		fmt.Fprintf(out, "warn  %v\n", werr)
	}
	pu := runPostUpgradeChecks(cmd.Context(), p, newRunner(), root, &d, target, info)
	if !pu.reached {
		fmt.Fprintf(out, "warn  %s\n", pu.results[0].Detail)
		return fail(nil, "post-upgrade verification could not reach the instance")
	}
	if verr := renderDoctor(out, pu.results); verr != nil {
		return fail(verr, "post-upgrade verification failed")
	}
	fmt.Fprintf(out, "upgraded %s: %s → %s — post-upgrade verification green\n", d.Project, old, target)
	return nil
}

// probeHealthz mirrors the compose healthcheck (the image ships node,
// not curl): exit 0 when the gateway answers /healthz with ok.
const probeHealthz = `node -e "fetch('http://localhost:18789/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"`

// upgradeWaitTimeout bounds the post-deploy readiness wait. A package
// var so tests can shrink it (a stubbed probe never turns healthy).
var upgradeWaitTimeout = 4 * time.Minute

// waitUpgradeReady gates the verify set on the new boot actually being
// done: gateway healthy first, then post-startup's completion marker
// recording the TARGET image (a warm volume's stale status.json must
// not short-circuit the wait).
func waitUpgradeReady(ctx context.Context, p platform.Platform, root string, d *platform.Deployment, target string) error {
	deadline := time.Now().Add(upgradeWaitTimeout)
	for {
		if _, err := p.Probe(ctx, newRunner(), root, d, probeHealthz); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instance not healthy in time — `agentctl status` / `agentctl logs`")
		}
		time.Sleep(2 * time.Second)
	}
	for {
		raw, err := p.Probe(ctx, newRunner(), root, d, probeStatus)
		if err == nil {
			var st struct {
				ImageVersion string `json:"imageVersion"`
			}
			if json.Unmarshal([]byte(raw), &st) == nil && st.ImageVersion == target {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("post-startup did not record image %s in time — cron may not be seeded; `agentctl logs`", target)
		}
		time.Sleep(2 * time.Second)
	}
}

func printUpgradePlan(out io.Writer, project, old, target string) {
	fmt.Fprintf(out, "upgrade plan for %s (%s → %s):\n", project, old, target)
	fmt.Fprintln(out, "  1. gate: doctor --target preview (era crossings, backup readiness, target spec gate)")
	fmt.Fprintln(out, "  2. backup: verified backup inside the running instance")
	fmt.Fprintln(out, "  3. rewrite: agent/Dockerfile FROM ...:"+old+" → ...:"+target)
	fmt.Fprintln(out, "  4. deploy: converge the stack onto the new image")
	fmt.Fprintln(out, "  5. verify: doctor --post-upgrade (marker, backup, mcp, cron, status)")
}

// confirmUpgrade asks the operator to type the NEW tag — explicit
// intent for the target, mirroring confirmDestroy's type-to-confirm.
func confirmUpgrade(in io.Reader, out io.Writer, project, old, target string) error {
	fmt.Fprintf(out, "upgrade %s: base image %s → %s — type the new tag to confirm: ", project, old, target)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(line) != target {
		return fmt.Errorf("aborted — nothing was changed")
	}
	return nil
}
