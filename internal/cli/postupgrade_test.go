package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/project"
)

// postProbeKey mirrors the compose adapter's exec argv so runOutputs
// entries hit the same joined key the stubRunner records.
func postProbeKey(command string) string {
	return "podman compose -f compose.yml exec -T agent sh -c " + command
}

// greenPostUpgradeOutputs scripts every D12 probe for a healthy
// post-upgrade instance running the fixture's pinned 2026.09.05: fresh
// this-boot backup, empty MCP surface, seeded cron job, clean status
// summary, no pending heal retries.
func greenPostUpgradeOutputs() map[string]string {
	return map[string]string{
		postProbeKey(probeMarker):   "2026.09.05\n",
		postProbeKey(probeBackups):  "now=1757700000 uptime=3600\n-rw-r--r-- 1 node node 1024 1757699000 openclaw-backup-x.tar.gz\n",
		postProbeKey(probeMcpList):  `{"servers":[]}`,
		postProbeKey(probeCronList): `{"jobs":[{"name":"jobs"}]}`,
		postProbeKey(probeStatus):   `{"imageVersion":"2026.09.05","warnings":0,"bootCompletedAt":"2026-09-13T10:00:00+00:00"}`,
	}
}

func TestNewestBackupThisBoot(t *testing.T) {
	tests := []struct {
		name      string
		listing   string
		wantName  string
		wantFresh bool
		wantErr   bool
	}{
		{
			name:      "archive written this boot",
			listing:   "now=1757700000 uptime=3600\n-rw-r--r-- 1 node node 1024 1757699000 openclaw-backup-x.tar.gz\n",
			wantName:  "openclaw-backup-x.tar.gz",
			wantFresh: true,
		},
		{
			name:     "newest archive predates the boot",
			listing:  "now=1757700000 uptime=3600\ntotal 4\n-rw-r--r-- 1 node node 1024 1757600000 old.tar.gz\n-rw-r--r-- 1 node node 512 1757500000 older.tar.gz\n",
			wantName: "old.tar.gz",
		},
		{
			name:    "empty backups dir",
			listing: "now=1757700000 uptime=3600\n",
		},
		{
			name:    "missing header",
			listing: "-rw-r--r-- 1 node node 1024 1757699000 x.tar.gz\n",
			wantErr: true,
		},
		{
			name:     "archive name with spaces survives the join",
			listing:  "now=1757700000 uptime=60\n-rw-r--r-- 1 node node 1024 1757699999 backup of sunday.tar.gz\n",
			wantName: "backup of sunday.tar.gz", wantFresh: true,
		},
	}
	for _, tt := range tests {
		name, fresh, err := newestBackupThisBoot(tt.listing)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got %q/%v", tt.name, name, fresh)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tt.name, err)
			continue
		}
		if name != tt.wantName || fresh != tt.wantFresh {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tt.name, name, fresh, tt.wantName, tt.wantFresh)
		}
	}
}

func TestMcpListingNames(t *testing.T) {
	tests := []struct {
		name string
		json string
		want map[string]bool
		err  bool
	}{
		{"envelope name entries", `{"servers":[{"name":"acme"},{"name":"beta"}]}`, map[string]bool{"acme": true, "beta": true}, false},
		{"envelope string entries", `{"servers":["kept"]}`, map[string]bool{"kept": true}, false},
		{"name-keyed object", `{"acme":{"url":"x"},"extra":{}}`, map[string]bool{"acme": true, "extra": true}, false},
		{"bare list", `[{"name":"a"},"b"]`, map[string]bool{"a": true, "b": true}, false},
		{"unparseable", "not json", nil, true},
		{"unexpected shape", `42`, nil, true},
	}
	for _, tt := range tests {
		got, err := mcpListingNames(tt.json)
		if tt.err {
			if err == nil {
				t.Errorf("%s: expected error, got %v", tt.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tt.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCronListingNames(t *testing.T) {
	tests := []struct {
		name string
		json string
		want map[string]bool
		err  bool
	}{
		{"jobs envelope", `{"jobs":[{"name":"daily-briefing"},{"name":"sweep"}]}`, map[string]bool{"daily-briefing": true, "sweep": true}, false},
		{"data envelope", `{"data":[{"name":"daily-briefing"}]}`, map[string]bool{"daily-briefing": true}, false},
		{"bare list", `[{"name":"daily-briefing"}]`, map[string]bool{"daily-briefing": true}, false},
		{"empty jobs", `{"jobs":[]}`, map[string]bool{}, false},
		{"unparseable", "{", nil, true},
	}
	for _, tt := range tests {
		got, err := cronListingNames(tt.json)
		if tt.err {
			if err == nil {
				t.Errorf("%s: expected error, got %v", tt.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tt.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestEnvActiveMcpNames(t *testing.T) {
	root := fixtureProject(t)
	info := project.SpecInfo{
		AuthChoice: "zai-coding-global",
		McpServers: []project.SpecMcpServer{
			{Name: "filesystem"},
			{Name: "sentiment", IfEnv: []string{"SENTIMENT_API_KEY"}},
		},
	}
	got, ok := envActiveMcpNames(root, info)
	if !ok {
		t.Fatal("envActiveMcpNames unexpectedly reported .env unreadable")
	}
	// fixture .env sets FALLBACK_MODEL + ZAI_API_KEY, so the guarded
	// sentiment server drops out.
	if want := []string{"filesystem"}; !reflect.DeepEqual(got, want) {
		t.Errorf("envActiveMcpNames = %v, want %v", got, want)
	}
}

func TestAutomationJobNames(t *testing.T) {
	root := fixtureProject(t)
	if got := automationJobNames(root); !reflect.DeepEqual(got, []string{"jobs"}) {
		t.Errorf("automationJobNames = %v, want [jobs]", got)
	}
	if got := automationJobNames(filepath.Join(root, "nonexistent")); got != nil {
		t.Errorf("automationJobNames on a missing dir = %v, want nil", got)
	}
}

func TestDoctorPostUpgrade(t *testing.T) {
	t.Run("green verify names every check", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = greenPostUpgradeOutputs()
		out, err := execIn(t, root, "doctor", "--post-upgrade")
		if err != nil {
			t.Fatalf("doctor --post-upgrade errored: %v\n%s", err, out)
		}
		for _, want := range []string{
			"ok    running image matches 2026.09.05 (last-image-version)",
			"ok    verified backup from this boot: openclaw-backup-x.tar.gz",
			"ok    no spec'd MCP servers; none registered",
			"ok    cron list carries all 1 seeded jobs: jobs",
			"ok    boot summary clean: image 2026.09.05, 0 warnings",
			"ok    no pending doctor-skill heal retries",
			"all checks passed",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "rollback runbook") {
			t.Errorf("green run must not print the rollback runbook:\n%s", out)
		}
	})

	t.Run("marker mismatch fails and names the runbook", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = greenPostUpgradeOutputs()
		r.runOutputs[postProbeKey(probeMarker)] = "2026.09.01\n"
		out, err := execIn(t, root, "doctor", "--post-upgrade")
		if err == nil {
			t.Fatalf("marker mismatch must exit non-zero:\n%s", out)
		}
		if !strings.Contains(out, "FAIL  running image is 2026.09.01, expected 2026.09.05 — the container predates the upgrade; agentctl deploy, then re-run") {
			t.Errorf("output lacks the marker FAIL line:\n%s", out)
		}
		if !strings.Contains(out, "rollback runbook: compose down") {
			t.Errorf("output lacks the rollback runbook footer:\n%s", out)
		}
		if strings.Contains(out, "all checks passed") {
			t.Errorf("failed run must not claim all checks passed:\n%s", out)
		}
	})

	t.Run("expect-tag overrides the default expectation", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = greenPostUpgradeOutputs()
		r.runOutputs[postProbeKey(probeMarker)] = "2026.09.12\n"
		out, err := execIn(t, root, "doctor", "--post-upgrade", "--expect-tag", "2026.09.12")
		if err != nil {
			t.Fatalf("doctor --post-upgrade --expect-tag errored: %v\n%s", err, out)
		}
		if !strings.Contains(out, "ok    running image matches 2026.09.12") {
			t.Errorf("output lacks the expect-tag match line:\n%s", out)
		}
	})

	t.Run("missing spec'd mcp server fails naming the fix", func(t *testing.T) {
		root := fixtureProject(t)
		writeSpecWithMcp(t, root)
		r := stubbedRunner(t, "podman")
		r.runOutputs = greenPostUpgradeOutputs()
		r.runOutputs[postProbeKey(probeMcpList)] = `{"servers":[]}`
		out, err := execIn(t, root, "doctor", "--post-upgrade")
		if err == nil {
			t.Fatalf("missing MCP server must exit non-zero:\n%s", out)
		}
		if !strings.Contains(out, `FAIL  MCP server "filesystem" is spec'd (env-active) but not registered — the boot reconcile self-heals; agentctl stop && agentctl start, then re-run`) {
			t.Errorf("output lacks the mcp FAIL line:\n%s", out)
		}
		// SENTIMENT_API_KEY is unset in the fixture .env, so its guard
		// legitimately skips registration — no FAIL for sentiment.
		if strings.Contains(out, `"sentiment"`) {
			t.Errorf("if_env-guarded server must not be expected:\n%s", out)
		}
	})

	t.Run("unseeded cron job fails naming the fix", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = greenPostUpgradeOutputs()
		r.runOutputs[postProbeKey(probeCronList)] = `{"jobs":[]}`
		out, err := execIn(t, root, "doctor", "--post-upgrade")
		if err == nil {
			t.Fatalf("unseeded cron job must exit non-zero:\n%s", out)
		}
		if !strings.Contains(out, `FAIL  cron job "jobs" is not seeded — post-startup seeds cron after the gateway starts; wait a minute or agentctl stop && agentctl start, then re-run`) {
			t.Errorf("output lacks the cron FAIL line:\n%s", out)
		}
	})

	t.Run("pending heal retries warn", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = greenPostUpgradeOutputs()
		r.runOutputs[postProbeKey(probeHealMarker)] = ""
		out, err := execIn(t, root, "doctor", "--post-upgrade")
		if err != nil {
			t.Fatalf("heal warn must stay green: %v\n%s", err, out)
		}
		if !strings.Contains(out, "warn  doctor-heal-attempts marker present") {
			t.Errorf("output lacks the heal warn line:\n%s", out)
		}
	})

	t.Run("unreachable instance skips the verdict", func(t *testing.T) {
		root := fixtureProject(t)
		stubbedRunner(t, "podman")
		out, err := execIn(t, root, "doctor", "--post-upgrade")
		if err == nil {
			t.Fatalf("unreachable instance must exit non-zero:\n%s", out)
		}
		if !strings.Contains(out, "warn  instance not reachable — run `agentctl deploy`, then re-run doctor --post-upgrade") {
			t.Errorf("output lacks the unreachable warn:\n%s", out)
		}
		if strings.Contains(out, "all checks passed") {
			t.Errorf("skipped verification must not claim success:\n%s", out)
		}
	})

	t.Run("json renders the same checks", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = greenPostUpgradeOutputs()
		out, err := execIn(t, root, "doctor", "--post-upgrade", "--json")
		if err != nil {
			t.Fatalf("json post-upgrade errored: %v\n%s", err, out)
		}
		var report struct {
			Meta struct {
				Tag      string `json:"tag"`
				Platform string `json:"platform"`
			} `json:"meta"`
			Checks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"checks"`
			Failed bool `json:"failed"`
		}
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); jerr != nil {
			t.Fatalf("json output unparseable: %v\n%s", jerr, out)
		}
		if report.Failed {
			t.Errorf("green run reports failed=true:\n%s", out)
		}
		var names []string
		for _, c := range report.Checks {
			names = append(names, c.Name)
		}
		for _, want := range []string{"marker", "backup", "mcp", "cron", "status", "heal"} {
			found := false
			for _, n := range names {
				if n == want {
					found = true
				}
			}
			if !found {
				t.Errorf("json checks lack %q: %v", want, names)
			}
		}
		if report.Meta.Tag != "2026.09.05" {
			t.Errorf("json meta.tag = %q, want 2026.09.05", report.Meta.Tag)
		}
	})

	t.Run("flag misuse fails closed", func(t *testing.T) {
		root := fixtureProject(t)
		stubbedRunner(t, "podman")
		if _, err := execIn(t, root, "doctor", "--target", "2026.09.12", "--post-upgrade"); err == nil {
			t.Error("--target with --post-upgrade must error")
		}
		if _, err := execIn(t, root, "doctor", "--expect-tag", "2026.09.12"); err == nil {
			t.Error("--expect-tag without --post-upgrade must error")
		}
		if _, err := execIn(t, root, "doctor", "--post-upgrade", "--expect-tag", "not-a-tag"); err == nil {
			t.Error("malformed --expect-tag must error")
		}
	})
}

func writeSpecWithMcp(t *testing.T, root string) {
	t.Helper()
	spec := `{
  "specVersion": 1,
  "setup": {"auth_choice": "zai-coding-global"},
  "mcp_servers": [
    {"name": "filesystem", "command": "fs-mcp"},
    {"name": "sentiment", "url": "https://mcp.example.com", "if_env": ["SENTIMENT_API_KEY"]}
  ]
}`
	if err := os.WriteFile(filepath.Join(root, "agent", "spec.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorTargetMigrationExplainers pins the --target instance
// explainers: the legacy-shape probes (warn only, doc anchor) and the
// litellm migration preconditions — the checklist fires for a
// non-litellm provider on a warm volume and nowhere else.
func TestDoctorTargetMigrationExplainers(t *testing.T) {
	t.Run("legacy journal dir explains its migration", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{
			postProbeKey(probeMarker):                            "2026.09.05\n",
			postProbeKey("test -d /home/node/.openclaw/journal"): "",
		}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "warn  legacy {data}/journal present — the wrapper's next boot moves it into workspace/journal and drops {data}/docs (docs/standard-agent.md#migrations)") {
			t.Errorf("output lacks the journal explainer:\n%s", out)
		}
		if strings.Contains(out, "current-state.json") {
			t.Errorf("state-file explainer fired without its shape:\n%s", out)
		}
	})

	t.Run("legacy state file explains its rename", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{
			postProbeKey(probeMarker): "2026.09.05\n",
			postProbeKey("test -f /home/node/.openclaw/workspace/journal/current-state.json"): "",
		}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "warn  legacy state file workspace/journal/current-state.json — the wrapper's next boot renames it to tent-state.json (docs/standard-agent.md#migrations)") {
			t.Errorf("output lacks the state-file explainer:\n%s", out)
		}
	})

	t.Run("litellm checklist on warm non-litellm volume", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{postProbeKey(probeMarker): "2026.09.05\n"}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		for _, want := range []string{
			"warn  before migrating: agentctl backup, then copy the archive off the volume",
			"warn  auth flips never re-run setup on a warm volume — the blessed path is agentctl destroy --volumes, secrets init, secrets check, deploy",
			"warn  adopt the litellm shape — litellm/ tree, the compose sidecar + model-net",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks checklist line %q:\n%s", want, out)
			}
		}
		if strings.Count(out, "docs/standard-agent.md#migrating-an-existing-agent-to-litellm") != 3 {
			t.Errorf("checklist must anchor every line once each:\n%s", out)
		}
	})

	t.Run("no checklist for litellm specs", func(t *testing.T) {
		root := litellmFixture(t)
		pinFixture(t, root, "2026.09.05")
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{postProbeKey(probeMarker): "2026.09.05\n"}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if strings.Contains(out, "litellm-migration/") {
			t.Errorf("litellm spec must not get the migration checklist:\n%s", out)
		}
	})

	t.Run("no checklist on a fresh volume", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{postProbeKey(probeMarker): "\n"}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if strings.Contains(out, "litellm-migration/") {
			t.Errorf("fresh volume must not get the migration checklist:\n%s", out)
		}
	})
}
