package cli

import (
	"testing"

	"github.com/tankdonut/agent-base/internal/project"
	"github.com/tankdonut/agent-base/internal/scaffold"
)

func crossingIDs(t *testing.T, from, to string, info *project.SpecInfo) []string {
	t.Helper()
	var ids []string
	for _, e := range eraCrossings(from, to, info) {
		ids = append(ids, e.ID)
	}
	return ids
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("crossings = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("crossings = %v, want %v", got, want)
		}
	}
}

func TestEraCrossings(t *testing.T) {
	zai := &project.SpecInfo{AuthChoice: "zai-coding-global"}
	other := &project.SpecInfo{AuthChoice: "anthropic"}
	tests := []struct {
		name string
		from string
		to   string
		info *project.SpecInfo
		want []string
	}{
		{
			name: "same-day .N follow-up boundary counts",
			from: "2026.08.23", to: "2026.08.23.1", info: nil,
			want: []string{"version-marker-upgrade-backup", "agent-managed-mcp-removal", "agent-managed-plugins-snapshot"},
		},
		{
			name: "second same-day follow-up counts",
			from: "2026.08.24.1", to: "2026.08.24.2", info: nil,
			want: []string{"config-presets", "mcp-passthrough-config", "agent-sync-reseed", "plugin-prune-optin", "log-format-openclaw"},
		},
		{
			name: "no-delta same-day boundary is empty",
			from: "2026.08.24.2", to: "2026.08.24.3", info: nil,
		},
		{
			name: "same tag is empty",
			from: "2026.09.12.1", to: "2026.09.12.1", info: nil,
		},
		{
			name: "malformed from matches nothing",
			from: "latest", to: "2026.08.24", info: nil,
		},
		{
			name: "malformed to matches nothing",
			from: "2026.08.23", to: "v2", info: nil,
		},
		{
			name: "downgrade matches nothing",
			from: "2026.09.12", to: "2026.08.23", info: nil,
		},
		{
			name: "nil spec info drops predicate-gated entries",
			from: "2026.08.22", to: "2026.08.23", info: nil,
			want: []string{"remote-mcp-argv-fix", "doctor-skills-heal", "schedule-validation-fail-closed", "reconciler-timeout-warns", "gh-cli-installed"},
		},
		{
			name: "zai spec includes the zai gate",
			from: "2026.08.22", to: "2026.08.23", info: zai,
			want: []string{"zai-key-load-gate", "remote-mcp-argv-fix", "doctor-skills-heal", "schedule-validation-fail-closed", "reconciler-timeout-warns", "gh-cli-installed"},
		},
		{
			name: "other spec drops the zai gate",
			from: "2026.08.22", to: "2026.08.23", info: other,
			want: []string{"remote-mcp-argv-fix", "doctor-skills-heal", "schedule-validation-fail-closed", "reconciler-timeout-warns", "gh-cli-installed"},
		},
		{
			name: "wide range nil info keeps predicate-less entries only",
			from: "2026.08.22", to: "2026.08.24", info: nil,
			want: []string{
				"remote-mcp-argv-fix", "doctor-skills-heal", "schedule-validation-fail-closed", "reconciler-timeout-warns", "gh-cli-installed",
				"version-marker-upgrade-backup", "agent-managed-mcp-removal", "agent-managed-plugins-snapshot",
				"job-tools-allowlist", "tools-deny-default", "cron-failure-alerts", "image-healthcheck", "baked-env-supervisor-contract", "boot-diagnostics-files", "features-gateway-auth", "config-path-templating", "optional-secret-deferral",
			},
		},
		{
			name: "litellm boundary excludes litellm-gated entries for nil info",
			from: "2026.09.05", to: "2026.09.12", info: nil,
			want: []string{"gateway-bind-lan-seed", "npm-cache-readonly-fix", "model-thinking-field"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertIDs(t, crossingIDs(t, tt.from, tt.to, tt.info), tt.want)
		})
	}
}

func TestEraTableInvariants(t *testing.T) {
	seen := map[string]bool{}
	for i, e := range eras {
		if e.ID == "" || e.Summary == "" || e.Action == "" {
			t.Fatalf("eras[%d]: empty ID, Summary, or Action", i)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate era id %q", e.ID)
		}
		seen[e.ID] = true
		if e.Severity != StatusWarn && e.Severity != StatusFail {
			t.Fatalf("eras[%d] (%s): severity must be warn or fail, got %q", i, e.ID, e.Severity)
		}
		if tagDate(e.Release).IsZero() {
			t.Fatalf("eras[%d] (%s): unparseable release %q", i, e.ID, e.Release)
		}
		if i > 0 && tagAfter(eras[i-1].Release, e.Release) {
			t.Fatalf("eras[%d] (%s): table must ascend — %s orders after %s", i, e.ID, eras[i-1].Release, e.Release)
		}
	}
}

func TestLitellmSeedAnchor(t *testing.T) {
	seed, ok := eraByID("litellm-baseurl-seed")
	if !ok {
		t.Fatal("litellm-baseurl-seed entry missing — the doctor litellm-era check anchors on it")
	}
	if got, want := seed.Day.Format("2006.01.02"), "2026.09.12"; got != want {
		t.Fatalf("anchor day = %s, want %s", got, want)
	}
}

// TestEraTableNotRotted pins the maintenance contract: entries never
// reference a release newer than the shipping scaffold.DefaultBaseTag,
// and the release constant itself stays a parseable date tag. Same-day
// gaps are legal — a genuinely no-delta release needs no entry;
// recording new eras is the release skill's checklist step.
func TestEraTableNotRotted(t *testing.T) {
	baseDay := tagDate(scaffold.DefaultBaseTag)
	if baseDay.IsZero() {
		t.Fatalf("scaffold.DefaultBaseTag %q does not parse as YYYY.MM.DD[.N]", scaffold.DefaultBaseTag)
	}
	newest := eras[0].Day
	for _, e := range eras[1:] {
		if e.Day.After(newest) {
			newest = e.Day
		}
	}
	if newest.After(baseDay) {
		t.Fatalf("newest era day %s is after DefaultBaseTag day %s — entries must not outpace the release constant",
			newest.Format("2006.01.02"), baseDay.Format("2006.01.02"))
	}
}
