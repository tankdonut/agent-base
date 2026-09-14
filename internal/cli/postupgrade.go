package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

const dataDir = "/home/node/.openclaw"

const probeMarker = "cat " + dataDir + "/last-image-version"
const probeBackups = "echo now=$(date +%s) uptime=$(cut -d. -f1 /proc/uptime); ls -l --time-style=+%s /backups 2>/dev/null"
const probeMcpList = "openclaw mcp list --json"
const probeCronList = "openclaw cron list --json"
const probeStatus = "cat " + dataDir + "/status.json"
const probeHealMarker = "test -f " + dataDir + "/doctor-heal-attempts"

const postUpgradeRollback = "rollback runbook: compose down → restore the newest /backups archive → revert the agent/Dockerfile tag → compose up (docs/standard-agent.md#upgrade-runbook)"

// postUpgradeExit is the JSON path's exit rule — same verdicts as the
// text path: skip error when unreachable, failure error naming the
// rollback runbook when any check FAILed.
func postUpgradeExit(pu postUpgradeOutcome) error {
	if !pu.reached {
		return fmt.Errorf("post-upgrade verification skipped — instance not reachable")
	}
	if anyFailed(pu.results) {
		return fmt.Errorf("post-upgrade verification failed — rollback per docs/standard-agent.md#upgrade-runbook")
	}
	return nil
}

// postUpgradeOutcome is the D12 verify set plus whether the instance
// answered at all — an unreachable instance renders its warn line and
// skips the verdict, because "could not verify" must never read as
// "passed".
type postUpgradeOutcome struct {
	results []CheckResult
	reached bool
}

// runPostUpgradeChecks probes the running instance per the upgrade
// runbook's verify step: image marker, this-boot backup, MCP + cron
// reconciliation against the repo's spec and automations, the boot
// summary, and pending doctor-skill heal retries.
func runPostUpgradeChecks(ctx context.Context, plat platform.Platform, r process.Runner, root string, deploy *platform.Deployment, expect string, info project.SpecInfo) postUpgradeOutcome {
	probe := func(command string) (string, error) {
		return plat.Probe(ctx, r, root, deploy, command)
	}

	marker, err := probe(probeMarker)
	if err != nil {
		return postUpgradeOutcome{results: []CheckResult{{
			Name:   "post-upgrade",
			Status: StatusWarn,
			Detail: "instance not reachable — run `agentctl deploy`, then re-run doctor --post-upgrade",
		}}}
	}

	var results []CheckResult
	add := func(name string, status CheckStatus, format string, a ...any) {
		results = append(results, CheckResult{
			Name:   name,
			Status: status,
			Detail: fmt.Sprintf(format, a...),
		})
	}

	running := strings.TrimSpace(marker)
	switch {
	case running == "":
		add("marker", StatusFail, "last-image-version marker is empty — the boot did not finish its upgrade phase; agentctl logs")
	case running != expect:
		add("marker", StatusFail, "running image is %s, expected %s — the container predates the upgrade; agentctl deploy, then re-run", running, expect)
	default:
		add("marker", StatusOK, "running image matches %s (last-image-version)", running)
	}

	if listing, berr := probe(probeBackups); berr != nil {
		add("backup", StatusWarn, "/backups listing failed (%v) — check the volume mount", berr)
	} else if name, fresh, perr := newestBackupThisBoot(listing); perr != nil {
		add("backup", StatusWarn, "could not parse the /backups listing — check it manually (%v)", perr)
	} else if fresh {
		add("backup", StatusOK, "verified backup from this boot: %s", name)
	} else {
		add("backup", StatusWarn, "no /backups archive from this boot — fine on a fresh volume; on a warm volume take one (agentctl backup)")
	}

	if expected, ok := envActiveMcpNames(root, info); !ok {
		add("mcp", StatusWarn, "could not read agent/.env to evaluate if_env guards — skipped the registration set check")
	} else if listing, merr := probe(probeMcpList); merr != nil {
		add("mcp", StatusFail, "openclaw mcp list failed (%v) — agentctl logs", merr)
	} else if names, perr := mcpListingNames(listing); perr != nil {
		add("mcp", StatusFail, "mcp list output unparseable (%v) — agentctl logs", perr)
	} else {
		var missing, extras []string
		for _, server := range expected {
			if !names[server] {
				missing = append(missing, server)
			}
		}
		for name := range names {
			if !sliceContains(expected, name) {
				extras = append(extras, name)
			}
		}
		sort.Strings(extras)
		for _, name := range missing {
			add("mcp/"+name, StatusFail, "MCP server %q is spec'd (env-active) but not registered — the boot reconcile self-heals; agentctl stop && agentctl start, then re-run", name)
		}
		switch {
		case len(extras) > 0:
			add("mcp", StatusWarn, "%d registered but not spec'd (removal is mcp_prune-gated): %s", len(extras), strings.Join(extras, ", "))
		case len(expected) > 0:
			add("mcp", StatusOK, "mcp list matches the spec (%d env-active: %s)", len(expected), strings.Join(expected, ", "))
		default:
			add("mcp", StatusOK, "no spec'd MCP servers; none registered")
		}
	}

	if expected := automationJobNames(root); len(expected) == 0 && dirExists(filepath.Join(root, "automations")) {
		add("cron", StatusWarn, "agent/automations exists but holds no .md specs — nothing to seed; check the tree")
	} else if listing, cerr := probe(probeCronList); cerr != nil {
		add("cron", StatusFail, "openclaw cron list failed (%v) — agentctl logs", cerr)
	} else if names, perr := cronListingNames(listing); perr != nil {
		add("cron", StatusFail, "cron list output unparseable (%v) — agentctl logs", perr)
	} else {
		var missing []string
		for _, job := range expected {
			if !names[job] {
				missing = append(missing, job)
			}
		}
		if len(missing) > 0 {
			for _, job := range missing {
				add("cron/"+job, StatusFail, "cron job %q is not seeded — post-startup seeds cron after the gateway starts; wait a minute or agentctl stop && agentctl start, then re-run", job)
			}
		} else if len(expected) > 0 {
			add("cron", StatusOK, "cron list carries all %d seeded jobs: %s", len(expected), strings.Join(expected, ", "))
		} else {
			add("cron", StatusOK, "no automations to seed; cron list parsed (%d jobs)", len(names))
		}
	}

	if raw, serr := probe(probeStatus); serr != nil || strings.TrimSpace(raw) == "" {
		add("status", StatusWarn, "status.json absent — post-startup still running; re-run in a minute")
	} else {
		var st struct {
			ImageVersion string `json:"imageVersion"`
			Warnings     int    `json:"warnings"`
		}
		if jerr := json.Unmarshal([]byte(raw), &st); jerr != nil {
			add("status", StatusWarn, "status.json unparseable (%v) — check inside the instance", jerr)
		} else if st.Warnings > 0 {
			add("status", StatusWarn, "boot completed with %d warnings (image %s) — agentctl logs", st.Warnings, st.ImageVersion)
		} else {
			add("status", StatusOK, "boot summary clean: image %s, 0 warnings", st.ImageVersion)
		}
	}

	if _, herr := probe(probeHealMarker); herr == nil {
		add("heal", StatusWarn, "doctor-heal-attempts marker present — skill-heal retries are deferred until the next image change")
	} else {
		add("heal", StatusOK, "no pending doctor-skill heal retries")
	}

	return postUpgradeOutcome{results: results, reached: true}
}

// newestBackupThisBoot decides whether the newest /backups archive was
// written during the current container boot: the probe reports the
// in-container epoch, /proc/uptime (the boot's age), and an epoch-styled
// ls listing; an archive counts when its mtime is at or after the boot
// epoch (now − uptime). Comparing clocks inside the one container avoids
// host-clock skew entirely.
func newestBackupThisBoot(listing string) (string, bool, error) {
	now, uptime := -1, -1
	var rows []string
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if now < 0 {
			for _, field := range strings.Fields(line) {
				if v, err := strconv.Atoi(strings.TrimPrefix(field, "now=")); err == nil && strings.HasPrefix(field, "now=") {
					now = v
				}
				if v, err := strconv.Atoi(strings.TrimPrefix(field, "uptime=")); err == nil && strings.HasPrefix(field, "uptime=") {
					uptime = v
				}
			}
			continue
		}
		if !strings.HasPrefix(line, "total ") {
			rows = append(rows, line)
		}
	}
	if now < 0 || uptime < 0 {
		snippet := strings.TrimSpace(listing)
		if i := strings.IndexByte(snippet, '\n'); i >= 0 {
			snippet = snippet[:i]
		}
		return "", false, fmt.Errorf("missing now=/uptime= header in %q", snippet)
	}
	bootEpoch := now - uptime
	newest, newestName := 0, ""
	for _, row := range rows {
		fields := strings.Fields(row)
		if len(fields) < 7 {
			continue
		}
		mtime, err := strconv.Atoi(fields[5])
		if err != nil {
			continue
		}
		if mtime >= newest {
			newest, newestName = mtime, strings.Join(fields[6:], " ")
		}
	}
	if newest == 0 {
		return "", false, nil
	}
	return newestName, newest >= bootEpoch, nil
}

// mcpListingNames parses `openclaw mcp list --json`, mirroring the
// image's _mcp_listing_names: an enveloped {"servers": [...]} of names
// or {name: ...} objects, a name-keyed object, or a bare list — names
// are matched structurally, never as substrings.
func mcpListingNames(listing string) (map[string]bool, error) {
	var root any
	if err := json.Unmarshal([]byte(listing), &root); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	collect := func(items []any) {
		for _, entry := range items {
			switch t := entry.(type) {
			case string:
				names[t] = true
			case map[string]any:
				if name, ok := t["name"].(string); ok {
					names[name] = true
				}
			}
		}
	}
	switch t := root.(type) {
	case map[string]any:
		if servers, ok := t["servers"].([]any); ok {
			collect(servers)
			return names, nil
		}
		for name := range t {
			names[name] = true
		}
	case []any:
		collect(t)
	default:
		return nil, fmt.Errorf("unexpected mcp list shape %T", root)
	}
	return names, nil
}

// cronListingNames parses `openclaw cron list --json`: {"jobs": [...]}
// or {"data": [...]} envelopes or a bare list, each job a {"name": ...}
// object — the shape the image's reconciler matches against.
func cronListingNames(listing string) (map[string]bool, error) {
	var root any
	if err := json.Unmarshal([]byte(listing), &root); err != nil {
		return nil, err
	}
	jobs := []any{}
	switch t := root.(type) {
	case map[string]any:
		if raw, ok := t["jobs"].([]any); ok {
			jobs = raw
		} else if raw, ok := t["data"].([]any); ok {
			jobs = raw
		}
	case []any:
		jobs = t
	default:
		return nil, fmt.Errorf("unexpected cron list shape %T", root)
	}
	names := map[string]bool{}
	for _, job := range jobs {
		if entry, ok := job.(map[string]any); ok {
			if name, ok := entry["name"].(string); ok {
				names[name] = true
			}
		}
	}
	return names, nil
}

// envActiveMcpNames returns the spec'd server names whose if_env guards
// are all satisfied by agent/.env (unset guards skip registration at
// boot, so those servers are correctly absent from the live listing).
// ok=false means .env was unreadable and the set is unreliable.
func envActiveMcpNames(root string, info project.SpecInfo) ([]string, bool) {
	set := map[string]bool{}
	keys, err := project.EnvKeyNames(root)
	if err != nil {
		return nil, false
	}
	for _, k := range keys {
		set[k] = true
	}
	var active []string
	for _, server := range info.McpServers {
		skip := false
		for _, env := range server.IfEnv {
			if !set[env] {
				skip = true
				break
			}
		}
		if !skip {
			active = append(active, server.Name)
		}
	}
	return active, true
}

// automationJobNames lists the seeded job names — the .md file stems
// under agent/automations, which the image's loader requires to equal
// each job's declared name.
func automationJobNames(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, "automations"))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".md"))
	}
	sort.Strings(names)
	return names
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func sliceContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
