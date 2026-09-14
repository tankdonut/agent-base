package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/scaffold"
)

// The golden fixtures pin `agentctl doctor`'s text output byte-exactly:
// prefixes ("ok    ", "FAIL  ", "warn  "), line order, and the trailing
// verdict. The CheckResult model refactor must keep this output
// identical — representation changes, check semantics never do.

// doctorPassGolden is a contract-shaped non-litellm project on a host
// with an engine but without the pinned image: the migration advisory
// warns, the real-image spec gate skips, and the verdict still passes.
// Drift covers the three scaffold-contract files (the compose envelope
// is rendered, not scaffold-owned).
const doctorPassGolden = `ok    spec.json parses (1 env refs, 0 if_env guards)
ok    base image pinned: ghcr.io/tankdonut/agent-base:2026.09.05
ok    .env sets all 2 required vars
warn  provider "zai-coding-global" — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md "Migrating an existing agent to LiteLLM")
ok    platform "compose" manifest lints (project grow, 2 env keys set)
warn  differs from the scaffolded shape — keep deliberate edits, but reconcile against a fresh init tree before an upgrade; a non-default gateway port belongs in fleet.yaml (agents.<name>.gateway_port)
warn  missing — restore it from a fresh ` + "`agentctl init --force`" + ` tree
warn  missing — restore it from a fresh ` + "`agentctl init --force`" + ` tree
warn  pinned image ghcr.io/tankdonut/agent-base:2026.09.05 not local — skipped the real-image spec gate (run ` + "`agentctl deploy`" + ` once or pull it)
all checks passed
`

// doctorLitellmGolden is the full blessed litellm shape with the image
// local: every check lands ok and the gate runs.
const doctorLitellmGolden = `ok    spec.json parses (1 env refs, 0 if_env guards)
ok    base image pinned: ghcr.io/tankdonut/agent-base:2026.09.12
ok    .env sets all 2 required vars
ok    litellm sidecar shape present (tree, compose service, model-net)
ok    platform "compose" manifest lints (project grow, 3 env keys set)
warn  differs from the scaffolded shape — keep deliberate edits, but reconcile against a fresh init tree before an upgrade; a non-default gateway port belongs in fleet.yaml (agents.<name>.gateway_port)
warn  differs from the scaffolded shape — keep deliberate edits, but reconcile against a fresh init tree before an upgrade; a non-default gateway port belongs in fleet.yaml (agents.<name>.gateway_port)
warn  missing — restore it from a fresh ` + "`agentctl init --force`" + ` tree
ok    real-image spec gate passed via podman
all checks passed
`

// doctorEnginelessGolden is an engineless host: the platform construct
// fails naming the install fix, the gate skips, and doctor exits
// non-zero with the FAIL-lines error.
const doctorEnginelessGolden = `ok    spec.json parses (1 env refs, 0 if_env guards)
ok    base image pinned: ghcr.io/tankdonut/agent-base:2026.09.05
ok    .env sets all 2 required vars
warn  provider "zai-coding-global" — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md "Migrating an existing agent to LiteLLM")
FAIL  platform "compose": no container engine found — install podman or docker
warn  differs from the scaffolded shape — keep deliberate edits, but reconcile against a fresh init tree before an upgrade; a non-default gateway port belongs in fleet.yaml (agents.<name>.gateway_port)
warn  missing — restore it from a fresh ` + "`agentctl init --force`" + ` tree
warn  missing — restore it from a fresh ` + "`agentctl init --force`" + ` tree
warn  no compose engine — skipped the real-image spec gate
`

func TestDoctorTextGolden(t *testing.T) {
	tests := []struct {
		name    string
		project func(t *testing.T) string
		look    []string
		imageOK bool
		wantOut string
		wantErr string // "" means success
	}{
		{
			name:    "contract project, image not local",
			project: fixtureProject,
			look:    []string{"podman"},
			wantOut: doctorPassGolden,
		},
		{
			name: "litellm shape with local image",
			project: func(t *testing.T) string {
				return litellmFixture(t)
			},
			look:    []string{"podman"},
			imageOK: true,
			wantOut: doctorLitellmGolden,
		},
		{
			name:    "engineless host fails the platform construct",
			project: fixtureProject,
			look:    nil,
			wantOut: doctorEnginelessGolden,
			wantErr: "doctor found problems — fix the FAIL lines above",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.project(t)
			r := stubbedRunner(t, tt.look...)
			r.runOutputOK = tt.imageOK
			out, err := execIn(t, root, "doctor")
			if out != tt.wantOut {
				t.Errorf("doctor output drift:\n got: %q\nwant: %q", out, tt.wantOut)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("doctor errored: %v\n%s", err, out)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestDoctorJSONGolden pins the --json rendering: the same checks text
// doctor prints, compact single-line JSON with struct-marshaled field
// order, and unchanged exit semantics (a FAIL verdict still errors).
// The want-side marshals a literal report struct, so the golden locks
// the schema, not hand-escaped bytes.
func TestDoctorJSONGolden(t *testing.T) {
	meta := doctorMeta{Tag: "2026.09.05", Platform: "compose", Project: "grow"}
	specOK := CheckResult{Name: "spec", Status: StatusOK, Detail: "spec.json parses (1 env refs, 0 if_env guards)"}
	tagOK := CheckResult{Name: "base-tag", Status: StatusOK, Detail: "base image pinned: ghcr.io/tankdonut/agent-base:2026.09.05"}
	secretsOK := CheckResult{Name: "secrets", Status: StatusOK, Detail: ".env sets all 2 required vars"}
	litellmWarn := CheckResult{Name: "litellm-tree", Status: StatusWarn, Detail: `provider "zai-coding-global" — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md "Migrating an existing agent to LiteLLM")`}
	differs := "differs from the scaffolded shape — keep deliberate edits, but reconcile against a fresh init tree before an upgrade; a non-default gateway port belongs in fleet.yaml (agents.<name>.gateway_port)"
	missing := "missing — restore it from a fresh `agentctl init --force` tree"
	// The synthetic fixture is deliberately minimal: the value-bearing
	// contract files drift and the litellm pair is absent.
	fixtureDrift := []CheckResult{
		{Name: "template/.env.example", Status: StatusWarn, Detail: differs},
		{Name: "template/litellm/.env.example", Status: StatusWarn, Detail: missing},
		{Name: "template/litellm/config.yaml", Status: StatusWarn, Detail: missing},
	}
	tests := []struct {
		name    string
		look    []string
		want    doctorReport
		wantErr string
	}{
		{
			name: "contract project, image not local",
			look: []string{"podman"},
			want: doctorReport{Meta: meta, Checks: append([]CheckResult{
				specOK, tagOK, secretsOK, litellmWarn,
				{Name: "platform", Status: StatusOK, Detail: `platform "compose" manifest lints (project grow, 2 env keys set)`},
			}, append(append([]CheckResult{}, fixtureDrift...),
				CheckResult{Name: "spec-gate", Status: StatusWarn, Detail: "pinned image ghcr.io/tankdonut/agent-base:2026.09.05 not local — skipped the real-image spec gate (run `agentctl deploy` once or pull it)"},
			)...)},
		},
		{
			name: "engineless host fails the platform construct",
			look: nil,
			want: doctorReport{Meta: meta, Checks: append([]CheckResult{
				specOK, tagOK, secretsOK, litellmWarn,
				{Name: "platform", Status: StatusFail, Detail: `platform "compose": no container engine found — install podman or docker`},
			}, append(append([]CheckResult{}, fixtureDrift...),
				CheckResult{Name: "spec-gate", Status: StatusWarn, Detail: "no compose engine — skipped the real-image spec gate"},
			)...), Failed: true},
			wantErr: "doctor found problems — fix the FAIL lines above",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fixtureProject(t)
			r := stubbedRunner(t, tt.look...)
			r.runOutputOK = false
			out, err := execIn(t, root, "doctor", "--json")
			want, merr := json.Marshal(tt.want)
			if merr != nil {
				t.Fatal(merr)
			}
			if out != string(want)+"\n" {
				t.Errorf("doctor --json drift:\n got: %s\nwant: %s", out, want)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("doctor --json errored: %v\n%s", err, out)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestDoctorTemplateDrift pins the drift check against a real
// scaffold.Run tree: pristine output reports zero drift for both
// telegram shapes, a single contract-file edit flips exactly one check,
// and a non-default gateway port recorded in fleet.yaml changes the
// render (the recovery seam) instead of reading as false drift.
func TestDoctorTemplateDrift(t *testing.T) {
	scaffoldProject := func(t *testing.T, telegram bool) string {
		t.Helper()
		root := t.TempDir()
		name := filepath.Base(root)
		key := scaffold.ComposeProject(name)
		cfg := scaffold.Config{
			ProjectName: name,
			AgentName:   scaffold.DefaultAgentName(name),
			BaseTag:     scaffold.DefaultBaseTag,
			Model:       scaffold.DefaultModel,
			GatewayPort: scaffold.DefaultGatewayPort,
			Telegram:    telegram,
			TargetDir:   root,
		}
		if _, err := scaffold.Run(cfg); err != nil {
			t.Fatal(err)
		}
		// doctor's secrets check needs the env files; the scaffold
		// ships only .env.example (a litellm spec also requires
		// litellm/.env — SecretsCheck fails without it).
		scaffolded := filepath.Join(root, "agents", key)
		if err := os.WriteFile(filepath.Join(scaffolded, ".env"), []byte("LITELLM_API_KEY=mk\nTELEGRAM_ALLOWED_USERS=u\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scaffolded, "litellm", ".env"), []byte("LITELLM_MASTER_KEY=mk\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}
	driftPaths := func(root string) string {
		return filepath.Join(root, "agents", scaffold.ComposeProject(filepath.Base(root)))
	}
	drift := func(t *testing.T, root string) map[string]string {
		t.Helper()
		stubbedRunner(t, "podman")
		out, err := execIn(t, root, "doctor", "--json")
		if err != nil {
			t.Fatalf("doctor --json: %v\n%s", err, out)
		}
		var report doctorReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
			t.Fatalf("parsing report: %v\n%s", err, out)
		}
		got := map[string]string{}
		for _, c := range report.Checks {
			if strings.HasPrefix(c.Name, "template/") {
				got[c.Name] = string(c.Status)
			}
		}
		return got
	}

	for _, telegram := range []bool{true, false} {
		t.Run(map[bool]string{true: "pristine telegram", false: "pristine no-telegram"}[telegram], func(t *testing.T) {
			got := drift(t, scaffoldProject(t, telegram))
			if len(got) != 3 {
				t.Fatalf("want 3 template checks, got %v", got)
			}
			for name, status := range got {
				if status != "ok" {
					t.Errorf("%s = %s on a pristine scaffold, want ok", name, status)
				}
			}
		})
	}

	t.Run("one contract-file edit flips exactly one check", func(t *testing.T) {
		root := scaffoldProject(t, true)
		p := filepath.Join(driftPaths(root), ".env.example")
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(data, []byte("# deliberate drift\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		got := drift(t, root)
		if got["template/.env.example"] != "warn" {
			t.Errorf("template/.env.example = %s, want warn", got["template/.env.example"])
		}
		for _, name := range []string{"template/litellm/.env.example", "template/litellm/config.yaml"} {
			if got[name] != "ok" {
				t.Errorf("%s = %s, want ok (only the edited file should flip)", name, got[name])
			}
		}
	})

	t.Run("recorded non-default port keeps the render pristine", func(t *testing.T) {
		root := scaffoldProject(t, true)
		// Re-scaffold with a non-default port: init records it in
		// fleet.yaml, and drift recovery must follow the manifest,
		// not the scaffold default.
		cfg := scaffold.Config{
			ProjectName: filepath.Base(root),
			AgentName:   scaffold.DefaultAgentName(filepath.Base(root)),
			BaseTag:     scaffold.DefaultBaseTag,
			Model:       scaffold.DefaultModel,
			GatewayPort: 19001,
			Telegram:    true,
			TargetDir:   root,
			Force:       true,
		}
		if _, err := scaffold.Run(cfg); err != nil {
			t.Fatal(err)
		}
		got := drift(t, root)
		for name, status := range got {
			if status != "ok" {
				t.Errorf("%s = %s with the port recorded in fleet.yaml, want ok", name, status)
			}
		}
	})

	t.Run("missing litellm tree reports missing", func(t *testing.T) {
		root := scaffoldProject(t, true)
		if err := os.Remove(filepath.Join(driftPaths(root), "litellm", "config.yaml")); err != nil {
			t.Fatal(err)
		}
		got := drift(t, root)
		if got["template/litellm/config.yaml"] != "warn" {
			t.Errorf("template/litellm/config.yaml = %s, want warn", got["template/litellm/config.yaml"])
		}
		if got["template/.env.example"] != "ok" {
			t.Errorf("template/.env.example = %s, want ok (untouched)", got["template/.env.example"])
		}
	})
}

// TestDoctorTarget pins the --target era-crossings preview: crossings
// from the era table render as era/<id> lines carrying their own
// severity (a fail entry on an applicable spec FAILs the report),
// zero-crossing and downgrade targets get their own lines, malformed
// targets fail closed before any check runs, and --json carries
// meta.target.
func TestDoctorTarget(t *testing.T) {
	retag := func(t *testing.T, root, newTag string) {
		t.Helper()
		path := filepath.Join(agentDir(root), "Dockerfile")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritten := bytes.ReplaceAll(data, []byte(":2026.09.12"), []byte(":"+newTag))
		if err := os.WriteFile(path, rewritten, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("litellm spec crossing the seed boundary fails naming the era", func(t *testing.T) {
		root := litellmFixture(t)
		retag(t, root, "2026.09.05")
		r := stubbedRunner(t, "podman")
		r.runOutputOK = true
		out, err := execIn(t, root, "doctor", "--target", "2026.09.12")
		if err == nil {
			t.Fatalf("expected the era FAIL to error the report:\n%s", out)
		}
		for _, want := range []string{
			"FAIL  era/litellm-auth-gate: 2026.09.12: the loader gates on LITELLM_API_KEY",
			"warn  era/litellm-baseurl-seed: 2026.09.12:",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("target on the pinned tag reports no crossings", func(t *testing.T) {
		root := fixtureProject(t)
		stubbedRunner(t, "podman")
		out, err := execIn(t, root, "doctor", "--target", "2026.09.05")
		if err != nil {
			t.Fatalf("doctor --target errored: %v\n%s", err, out)
		}
		if !strings.Contains(out, "ok    no era crossings 2026.09.05 → 2026.09.05") {
			t.Errorf("output lacks the no-crossings line:\n%s", out)
		}
	})

	t.Run("downgrade target warns", func(t *testing.T) {
		root := fixtureProject(t)
		stubbedRunner(t, "podman")
		out, err := execIn(t, root, "doctor", "--target", "2026.08.23")
		if err != nil {
			t.Fatalf("downgrade preview must not error: %v\n%s", err, out)
		}
		if !strings.Contains(out, "downgrade crossings are not modeled") {
			t.Errorf("output lacks the downgrade warn:\n%s", out)
		}
	})

	t.Run("malformed target fails closed", func(t *testing.T) {
		root := fixtureProject(t)
		stubbedRunner(t, "podman")
		_, err := execIn(t, root, "doctor", "--target", "latest")
		if err == nil || !strings.Contains(err.Error(), "not a valid image tag") {
			t.Fatalf("err = %v, want invalid-tag error", err)
		}
	})

	t.Run("json carries meta.target", func(t *testing.T) {
		root := fixtureProject(t)
		stubbedRunner(t, "podman")
		out, err := execIn(t, root, "doctor", "--json", "--target", "2026.09.05")
		if err != nil {
			t.Fatalf("doctor --json --target errored: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"target":"2026.09.05"`) {
			t.Errorf("json lacks meta.target:\n%s", out)
		}
	})
}

// pinFixture rewrites the fixture Dockerfile's FROM tag.
func pinFixture(t *testing.T, root, tag string) {
	t.Helper()
	path := filepath.Join(agentDir(root), "Dockerfile")
	content := "FROM ghcr.io/tankdonut/agent-base:" + tag + "\nCOPY agent/spec.json /opt/agent/spec.json\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorTargetSpecGate pins the --target spec gate: it validates
// against the target ref (never the pinned one), a missing target
// image is pulled rather than skipped, and engineless hosts keep the
// warn-skip idiom without attempting anything. The fixture pins to the
// target's parent day so the crossings carry no FAIL entry.
func TestDoctorTargetSpecGate(t *testing.T) {
	sawCall := func(r *stubRunner, substrings ...string) bool {
		for _, c := range r.calls {
			joined := strings.Join(c, " ")
			found := true
			for _, s := range substrings {
				found = found && strings.Contains(joined, s)
			}
			if found {
				return true
			}
		}
		return false
	}

	t.Run("gate validates against the target ref when local", func(t *testing.T) {
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		r := stubbedRunner(t, "podman")
		r.runOutputOK = true
		out, err := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if err != nil {
			t.Fatalf("doctor --target errored: %v\n%s", err, out)
		}
		if !strings.Contains(out, "target spec gate passed via podman against ghcr.io/tankdonut/agent-base:2026.09.12.1") {
			t.Errorf("output lacks the target gate line:\n%s", out)
		}
		if !sawCall(r, "--validate-spec", "ghcr.io/tankdonut/agent-base:2026.09.12.1") {
			t.Errorf("validate never ran against the target ref: %v", r.calls)
		}
	})

	t.Run("missing target image is pulled then gated", func(t *testing.T) {
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		r := stubbedRunner(t, "podman")
		r.runOutputOK = false
		out, err := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if err != nil {
			t.Fatalf("doctor --target errored: %v\n%s", err, out)
		}
		if !strings.Contains(out, "target spec gate passed via podman (pulled ghcr.io/tankdonut/agent-base:2026.09.12.1)") {
			t.Errorf("output lacks the pulled-gate line:\n%s", out)
		}
		if !sawCall(r, "image pull ghcr.io/tankdonut/agent-base:2026.09.12.1") {
			t.Errorf("image pull never ran: %v", r.calls)
		}
	})

	t.Run("engineless host warns and never pulls", func(t *testing.T) {
		root := fixtureProject(t)
		r := stubbedRunner(t)
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12")
		if !strings.Contains(out, "warn  no compose engine — skipped the real-image spec gate") {
			t.Errorf("output lacks the engineless warn:\n%s", out)
		}
		if len(r.calls) != 0 {
			t.Errorf("engineless run must not exec anything: %v", r.calls)
		}
	})
}

// TestDoctorBackupReadiness pins the --target backup-readiness checks:
// the static compose /backups mount line (the fleet render always
// carries it — an envelope invariant, not an authored choice), and the
// volume probes over the Platform port (warm marker, migration-boot
// shape, fresh volume, unreachable instance).
func TestDoctorBackupReadiness(t *testing.T) {
	probeKey := func(command string) string {
		return "podman compose -f compose.yml exec -T agent sh -c " + command
	}
	withBackups := func(t *testing.T) string {
		t.Helper()
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		return root
	}

	t.Run("rendered envelope always mounts /backups", func(t *testing.T) {
		root := withBackups(t)
		stubbedRunner(t, "podman")
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "ok    named volume mounted at /backups — migration archives survive container replacement") {
			t.Errorf("output lacks the mount ok line:\n%s", out)
		}
	})

	t.Run("warm volume via marker", func(t *testing.T) {
		root := withBackups(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{
			probeKey("cat /home/node/.openclaw/last-image-version"): "2026.09.05\n",
			probeKey("df -h /backups"):                              "Filesystem  Size  Used Avail Use% Mounted on\noverlay  100G  20G  80G  20% /backups\n",
		}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "ok    warm volume — last migration marker 2026.09.05") {
			t.Errorf("output lacks the warm-volume line:\n%s", out)
		}
		if !strings.Contains(out, "ok    /backups: overlay  100G  20G  80G  20% /backups") {
			t.Errorf("output lacks the df line:\n%s", out)
		}
	})

	t.Run("migration boot next when openclaw.json present without marker", func(t *testing.T) {
		root := withBackups(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{
			probeKey("cat /home/node/.openclaw/last-image-version"): "\n",
			probeKey("test -f /home/node/.openclaw/openclaw.json"):  "",
		}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "warn  next boot is a migration boot") {
			t.Errorf("output lacks the migration-boot warn:\n%s", out)
		}
	})

	t.Run("fresh volume when both markers absent", func(t *testing.T) {
		root := withBackups(t)
		r := stubbedRunner(t, "podman")
		r.runOutputs = map[string]string{
			probeKey("cat /home/node/.openclaw/last-image-version"): "\n",
		}
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "ok    fresh volume — first boot runs setup; no migration involved") {
			t.Errorf("output lacks the fresh-volume line:\n%s", out)
		}
	})

	t.Run("unreachable instance warns and skips", func(t *testing.T) {
		root := withBackups(t)
		stubbedRunner(t, "podman")
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "warn  instance not reachable — skipped the volume probes") {
			t.Errorf("output lacks the unreachable warn:\n%s", out)
		}
	})
}
