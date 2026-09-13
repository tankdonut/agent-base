// Package-local era table: every dated, observable boot-behavior
// change in the agent-base image lineage, mined from the full release
// history (2026.08.22 → 2026.09.12.1). The release skill appends
// entries at release time; this table is the backfill.
//
// No-delta boundaries (adjacent tags with zero container/docs/examples
// delta — audited, deliberately no entries): 08.24→08.24.1 (CI only),
// 08.24.2→08.24.3 (CI only), 08.24.3→08.27 (CI only), 08.28→08.29
// (agentctl introduction), 08.29→08.29.1 (release bump).
//
// Investigated and excluded as unverifiable from this repo: the 08.28
// base-image digest re-pin (upstream tag movement unobservable here)
// and OPENCLAW_LOG_LEVEL (no observable change at the pinned tag).
package cli

import (
	"strconv"
	"strings"
	"time"

	"github.com/tankdonut/agent-base/internal/project"
)

// Era is one dated, observable boot-behavior change — the unit doctor
// reports when a pin crosses it. Severity fail means a boot on the far
// side can abort or break a workflow that worked on the near side;
// warn means changed-but-non-fatal behavior. Summary and Action are
// one operator-facing line each.
type Era struct {
	Release  string // full tag of the release carrying the change
	Day      time.Time
	ID       string
	Severity CheckStatus
	Summary  string
	Action   string
	// Applies gates the entry to projects whose spec shape matches;
	// nil applies to every project. Audience narrowing that depends on
	// runtime state (env vars, volume warmth) stays in Summary prose.
	Applies func(*project.SpecInfo) bool
}

func zaiCodingSpec(info *project.SpecInfo) bool {
	return strings.HasPrefix(info.AuthChoice, "zai-coding-")
}

func litellmSpec(info *project.SpecInfo) bool {
	return info.AuthChoice == "litellm-api-key"
}

// mustEra builds a table entry; a malformed release tag panics — the
// table is compile-time data and a bad date is a programming error.
func mustEra(release, id string, severity CheckStatus, summary, action string, applies func(*project.SpecInfo) bool) Era {
	day := tagDate(release)
	if day.IsZero() {
		panic("eras: bad release " + release)
	}
	return Era{Release: release, Day: day, ID: id, Severity: severity, Summary: summary, Action: action, Applies: applies}
}

// eras lists every entry ascending by (day, run suffix). Multiple
// entries may share a release (several observable changes shipped
// together) and a day (same-day .N follow-ups).
var eras = []Era{
	mustEra("2026.08.23", "zai-key-load-gate", StatusFail,
		"the loader fails closed naming ZAI_API_KEY when absent; first-boot setup aborts cleanly instead of crash-looping",
		"set ZAI_API_KEY in the deployment env before upgrading", zaiCodingSpec),
	mustEra("2026.08.23", "remote-mcp-argv-fix", StatusWarn,
		"remote MCP registration argv fixed (--type remote dropped, k=v headers) and a timeout key added; broken older registrations heal on the next boot",
		"none — older remote entries re-register correctly", nil),
	mustEra("2026.08.23", "doctor-skills-heal", StatusWarn,
		"skills auto-disabled by the in-image doctor re-enable once the finding clears (marker {data}/doctor-disabled-skills); operator-set enabled=false is never overridden",
		"none — expect disable/heal writes on first boot across the boundary", nil),
	mustEra("2026.08.23", "schedule-validation-fail-closed", StatusFail,
		"invalid every:/cron: automations headers abort --validate-spec and cron seeding pre-mutation; previously they seeded jobs that cron-edited every boot forever",
		"run --validate-spec against the new image before upgrading", nil),
	mustEra("2026.08.23", "reconciler-timeout-warns", StatusWarn,
		"every reconciler CLI spawn carries a 60s timeout (synthetic exit 124) — a hung CLI no longer hangs post_startup; failed local-plugin installs warn",
		"none", nil),
	mustEra("2026.08.23", "gh-cli-installed", StatusWarn,
		"gh is baked into the image — AGENT_GIT_TOKEN auth actually applies now (it warned non-functionally every boot before)",
		"none", nil),
	mustEra("2026.08.23.1", "version-marker-upgrade-backup", StatusFail,
		"a verified openclaw backup runs before any mutating phase on an AGENT_BASE_VERSION delta; backup failure aborts the boot (exit 1, retried next boot); fresh volumes just record the marker",
		"mount a writable volume at /backups before upgrading", nil),
	mustEra("2026.08.23.1", "agent-managed-mcp-removal", StatusWarn,
		"base-registered MCP servers that left the spec are mcp-unset via the {data}/agent-managed-mcp marker; operator-registered servers are untouched; failed removals warn and retry",
		"none — manual mcp unset surgery on image bumps is obsolete", nil),
	mustEra("2026.08.23.1", "agent-managed-plugins-snapshot", StatusWarn,
		"first boot snapshots base-installed plugins to {data}/agent-managed-plugins; undeclared registry plugins get a warn-only orphan report",
		"expect one orphan-report warn per undeclared plugin on volumes from older images", nil),
	mustEra("2026.08.24", "job-tools-allowlist", StatusWarn,
		"seeded cron jobs run with a bounded tool allow-list (fs/runtime/web/memory + bundle-mcp) where they previously had none; per-job tools: header, automations.default_tools, or * overrides",
		"declare tools:/default_tools for automations needing more than the bounded roster", nil),
	mustEra("2026.08.24", "tools-deny-default", StatusWarn,
		"the base seeds tools.deny = [cron, subagents, sessions_spawn, nodes] unless an env-active spec tools.* entry exists — agent turns lose the cron self-modification surface",
		"declare a spec tools.* entry to own tool policy from then on", nil),
	mustEra("2026.08.24", "cron-failure-alerts", StatusWarn,
		"seeded jobs attach --failure-alert + include-skipped when TELEGRAM_CHAT_ID is set; no chat means jobs run without alerts",
		"set TELEGRAM_CHAT_ID to receive missed-run alerts", nil),
	mustEra("2026.08.24", "image-healthcheck", StatusWarn,
		"image HEALTHCHECK probes no-auth /healthz on :18789 (start_period 300s); OCI-format builds drop it",
		"none for docker-format builds; podman builders need --format docker", nil),
	mustEra("2026.08.24", "baked-env-supervisor-contract", StatusWarn,
		"image bakes OPENCLAW_SERVICE_REPAIR_POLICY=external and OPENCLAW_NO_AUTO_UPDATE=1 — the in-image doctor never restarts services and the image never self-updates",
		"none — the host restart policy owns restarts", nil),
	mustEra("2026.08.24", "boot-diagnostics-files", StatusWarn,
		"new boot artifacts: {data}/logs/doctor-report.json and security-report.json when findings exist, {data}/status.json boot summary (warning text never persisted)",
		"point monitoring at {data}/status.json", nil),
	mustEra("2026.08.24", "features-gateway-auth", StatusWarn,
		"features.gateway_auth: true installs the gateway auth pair from OPENCLAW_GATEWAY_TOKEN — replacing the hand-rolled spec config pair",
		"replace the hand-rolled config pair with the flag + env", nil),
	mustEra("2026.08.24", "config-path-templating", StatusFail,
		"config path KEYS template-resolve at load, fail-closed — an unresolvable path ref aborts the load naming the variable",
		"ensure path-referenced env vars are set before upgrading", nil),
	mustEra("2026.08.24", "optional-secret-deferral", StatusWarn,
		"if_env-unsatisfied entries defer {env:...} resolution (inert, skipped at reconcile) instead of aborting — optional integrations become genuinely optional; unguarded refs still abort",
		"none", nil),
	mustEra("2026.08.24.2", "config-presets", StatusWarn,
		"new presets table + {\"include\": name} config splicing at load, fail-closed on unknown names and ambiguous shapes",
		"optional dedup surface — no change for specs not using it", nil),
	mustEra("2026.08.24.2", "mcp-passthrough-config", StatusWarn,
		"per-server config object applied every boot via config set --strict-json (heals drift, defers under unsatisfied if_env); key validity is the operator's responsibility",
		"verify knob names against the CLI", nil),
	mustEra("2026.08.24.2", "agent-sync-reseed", StatusWarn,
		"AGENT_SYNC=1 gates a one-boot force-reseed of workspace persona files; agent-written files outside the seed set survive",
		"use for one recovery boot only — never leave it set", nil),
	mustEra("2026.08.24.2", "plugin-prune-optin", StatusWarn,
		"features.plugin_prune: true uninstalls de-specified base-installed plugins (ownership marker maintained only while the flag is on; late enable catches up)",
		"opt in deliberately; default off", nil),
	mustEra("2026.08.24.2", "log-format-openclaw", StatusWarn,
		"boot logs reformat to <ISO-8601 UTC ms> [agent-entry] [info|warn] msg (was [agent-entry] msg / [agent-entry] WARNING: msg)",
		"update log scraping keyed on the old prefixes", nil),
	mustEra("2026.08.28", "graceful-shutdown-drain", StatusWarn,
		"supervise() replaces the execvp handoff: the CMD runs in its own process group; SIGTERM forwards to the CMD pid then drains the group up to AGENT_SHUTDOWN_GRACE (default 600s; 0 = immediate force-kill); a second signal force-kills; exit code preserved",
		"size stop timeouts to AGENT_SHUTDOWN_GRACE — in-flight automations now finish on docker stop", nil),
	mustEra("2026.08.28", "mcp-prune-gated", StatusWarn,
		"BEHAVIOR REVERSAL — base-registered MCP removal now requires features.mcp_prune: true (default off); without it de-specified servers stay registered and each boot warns",
		"set features.mcp_prune: true to keep removal semantics", nil),
	mustEra("2026.08.28", "gateway-auth-missing-token-warn", StatusWarn,
		"features.gateway_auth without OPENCLAW_GATEWAY_TOKEN warns once 'gateway auth pair NOT applied'; the misleading 'Applying gateway auth pair' line is gone",
		"set OPENCLAW_GATEWAY_TOKEN or drop the flag", nil),
	mustEra("2026.08.28", "mcp-plugin-exists-json-parse", StatusWarn,
		"existence checks parse JSON listings instead of substring-matching raw stdout — entries named like envelope keys (e.g. a server named \"servers\") register correctly now",
		"none — affected entries start registering", nil),
	mustEra("2026.08.28", "tools-deny-active-guard-only", StatusWarn,
		"spec_owns_tools counts only env-ACTIVE tools.* entries — a never-firing if_env guard no longer suppresses the base tools.deny default",
		"declare a real tools.* entry or accept the deny default", nil),
	mustEra("2026.08.28", "seed-content-symlink-guard", StatusWarn,
		"seed_content refuses symlinked/non-directory seeded roots with a warning instead of crash-looping through main() or writing through the symlink",
		"replace symlinks at seeded roots with real directories", nil),
	mustEra("2026.08.29.2", "remote-mcp-oauth-keys", StatusWarn,
		"first-class auth/oauth keys (identity/scope/authProfileId) on remote MCP entries, reconciled after registration every boot (heals drift)",
		"migrate hand-rolled mcp.servers.<name>.oauth knobs to the first-class keys", nil),
	mustEra("2026.08.29.2", "memory-index-force-fix", StatusWarn,
		"--force moved onto the memory index subcommand (was group-level, rejected by the CLI) — forced full reindex actually rebuilds now",
		"none", nil),
	mustEra("2026.08.31", "mcp-flag-drift-reregister", StatusWarn,
		"spec edits to MCP entries (auth header, URL, timeout, rotated {env:} values) propagate — drifted servers unset and re-add via the {data}/agent-mcp-args digest",
		"expect one-time re-registration churn for drifted servers on first boot across", nil),
	mustEra("2026.08.31", "mcp-transport-key", StatusWarn,
		"fail-closed transport: sse|streamable-http key on remote entries — POST-only endpoints rejected the CLI's SSE default with 405; rides the args digest",
		"add transport: streamable-http to POST-only remote servers", nil),
	mustEra("2026.08.31", "stable-skills-reconcile", StatusWarn,
		"skills disables require the finding in two settle-spaced doctor runs (kills the ~35-writes disable/heal oscillation per restart); heal retries deferred until the image changes",
		"none — restart churn disappears", nil),
	mustEra("2026.08.31", "env-native-gateway-auth", StatusWarn,
		"OPENCLAW_GATEWAY_TOKEN is the gateway's winning native surface — the base stops writing the legacy config pair and retires stale keys via config unset (spec-owned paths exempt)",
		"keep the env set; the legacy pair disappears from openclaw.json after first boot across", nil),
	mustEra("2026.08.31", "plugins-allow-seed", StatusWarn,
		"plugins.allow seeded as base-plugin-snapshot union spec plugins when nothing owns the path; existing/operator-owned paths are never clobbered",
		"review the seeded allowlist if you gate plugins", nil),
	mustEra("2026.08.31", "doctor-finding-detail-lines", StatusWarn,
		"doctor/security count lines gain up to ten per-finding checkId+path detail lines (never message text — secrets discipline)",
		"update boot-log expectations", nil),
	mustEra("2026.09.05", "automation-model-header", StatusWarn,
		"per-job model: header overrides automations.model, and changing the global now heals already-seeded jobs via cron edit --model",
		"expect one cron edit per seeded job on first boot across when the global model changed", nil),
	mustEra("2026.09.05", "trigger-script-header", StatusFail,
		"trigger-script: automations abort seeding unless AGENT_AUTOMATION_TRIGGERS=1; trigger evaluation runs with the owning agent's FULL tool policy, hence the deliberate opt-in; drift heals via cron edit",
		"set AGENT_AUTOMATION_TRIGGERS=1 deliberately before shipping trigger automations", nil),
	mustEra("2026.09.07", "gateway-bind-lan-seed", StatusWarn,
		"gateway.bind=lan seeded unless a spec entry owns the path — published ports become reachable (engines forward to the container interface, not loopback)",
		"own gateway.bind in the spec to keep loopback-only binding", nil),
	mustEra("2026.09.07", "npm-cache-readonly-fix", StatusFail,
		"NPM_CONFIG_CACHE=/home/node/.openclaw/.npm baked — first-boot plugin installs survive a read-only root filesystem (previously failed outside {data})",
		"none for read-only deployments after upgrade; other boots unaffected", nil),
	mustEra("2026.09.12", "litellm-auth-gate", StatusFail,
		"the loader gates on LITELLM_API_KEY (exact mirror of the zai gate); a missing var aborts the load naming it",
		"set LITELLM_API_KEY before upgrading litellm-api-key specs", litellmSpec),
	mustEra("2026.09.12", "litellm-baseurl-seed", StatusWarn,
		"models.providers.litellm.baseUrl=http://litellm:4000 seeded unless a spec entry owns the path — the CLI's loopback default is unreachable when the proxy is a compose sibling",
		"run the LiteLLM sidecar (compose prod template) or own the baseUrl path in the spec", litellmSpec),
	mustEra("2026.09.12", "model-thinking-field", StatusWarn,
		"new optional spec key model.thinking seeds agents.defaults.thinkingDefault",
		"optional — set it to control the default thinking level", nil),
	mustEra("2026.09.12.1", "models-allowlist-merge-seed", StatusWarn,
		"spec-referenced models + the GLM 5.3 series merge into an EXISTING agents.defaults.models allowlist (never creates one; spec-owned paths stand down) — fixes cron payload.model rejections from a stale baked list",
		"previously-rejected model refs start working; own agents.defaults.models in the spec to opt out", nil),
}

// runSuffix reads the optional .N part of a tag; absent or non-numeric
// counts as 0 (the date dominates ordering either way).
func runSuffix(tag string) int {
	parts := strings.SplitN(tag, ".", 4)
	if len(parts) < 4 || parts[3] == "" {
		return 0
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return 0
	}
	return n
}

// tagAfter reports whether tag a orders strictly after tag b — date
// prefix first, then the .N run suffix. Malformed tags never order.
func tagAfter(a, b string) bool {
	ad, bd := tagDate(a), tagDate(b)
	switch {
	case ad.IsZero() || bd.IsZero():
		return false
	case ad.After(bd):
		return true
	case ad.Before(bd):
		return false
	default:
		return runSuffix(a) > runSuffix(b)
	}
}

// eraCrossings returns every era with Release strictly after from and
// at or before to (full-tag ordering, so same-day .N follow-ups count)
// whose Applies predicate passes info; a nil info keeps only
// predicate-less entries. Malformed tags match nothing — callers gate
// on parse success and say so. A downgrade (to older than from)
// matches nothing by design.
func eraCrossings(from, to string, info *project.SpecInfo) []Era {
	if tagDate(from).IsZero() || tagDate(to).IsZero() {
		return nil
	}
	var crossed []Era
	for _, e := range eras {
		if !tagAfter(e.Release, from) || tagAfter(e.Release, to) {
			continue
		}
		if e.Applies != nil && (info == nil || !e.Applies(info)) {
			continue
		}
		crossed = append(crossed, e)
	}
	return crossed
}

// eraByID finds an entry by ID; the litellm-era doctor check anchors
// on litellm-baseurl-seed's day.
func eraByID(id string) (Era, bool) {
	for _, e := range eras {
		if e.ID == id {
			return e, true
		}
	}
	return Era{}, false
}
