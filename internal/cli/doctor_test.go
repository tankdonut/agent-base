package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden fixtures pin `agentctl doctor`'s text output byte-exactly:
// prefixes ("ok    ", "FAIL  ", "warn  "), line order, and the trailing
// verdict. The CheckResult model refactor must keep this output
// identical — representation changes, check semantics never do.

// doctorPassGolden is a contract-shaped non-litellm project on a host
// with an engine but without the pinned image: the migration advisory
// warns, the real-image spec gate skips, and the verdict still passes.
const doctorPassGolden = `ok    spec.json parses (1 env refs, 0 if_env guards)
ok    base image pinned: ghcr.io/tankdonut/agent-base:2026.09.05
ok    agent/.env sets all 2 required vars
warn  provider "zai-coding-global" — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md "Migrating an existing agent to LiteLLM")
ok    platform "compose" manifest lints (project fixture-agent, 2 env keys set)
warn  pinned image ghcr.io/tankdonut/agent-base:2026.09.05 not local — skipped the real-image spec gate (run ` + "`agentctl deploy`" + ` once or pull it)
all checks passed
`

// doctorLitellmGolden is the full blessed litellm shape with the image
// local: every check lands ok and the gate runs.
const doctorLitellmGolden = `ok    spec.json parses (1 env refs, 0 if_env guards)
ok    base image pinned: ghcr.io/tankdonut/agent-base:2026.09.12
ok    agent/.env sets all 2 required vars
ok    litellm sidecar shape present (tree, compose service, model-net)
ok    platform "compose" manifest lints (project fixture-agent, 3 env keys set)
ok    real-image spec gate passed via podman
all checks passed
`

// doctorEnginelessGolden is an engineless host: the platform construct
// fails naming the install fix, the gate skips, and doctor exits
// non-zero with the FAIL-lines error.
const doctorEnginelessGolden = `ok    spec.json parses (1 env refs, 0 if_env guards)
ok    base image pinned: ghcr.io/tankdonut/agent-base:2026.09.05
ok    agent/.env sets all 2 required vars
warn  provider "zai-coding-global" — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md "Migrating an existing agent to LiteLLM")
FAIL  platform "compose": no container engine found — install podman or docker
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
				root := litellmFixture(t)
				addLitellmSidecar(t, root)
				return root
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
	meta := doctorMeta{Tag: "2026.09.05", Platform: "compose", Project: "fixture-agent"}
	specOK := CheckResult{Name: "spec", Status: StatusOK, Detail: "spec.json parses (1 env refs, 0 if_env guards)"}
	tagOK := CheckResult{Name: "base-tag", Status: StatusOK, Detail: "base image pinned: ghcr.io/tankdonut/agent-base:2026.09.05"}
	secretsOK := CheckResult{Name: "secrets", Status: StatusOK, Detail: "agent/.env sets all 2 required vars"}
	litellmWarn := CheckResult{Name: "litellm-tree", Status: StatusWarn, Detail: `provider "zai-coding-global" — litellm sidecar not adopted; the blessed migration is a fresh-volume path (docs/standard-agent.md "Migrating an existing agent to LiteLLM")`}
	tests := []struct {
		name    string
		look    []string
		want    doctorReport
		wantErr string
	}{
		{
			name: "contract project, image not local",
			look: []string{"podman"},
			want: doctorReport{Meta: meta, Checks: []CheckResult{
				specOK, tagOK, secretsOK, litellmWarn,
				{Name: "platform", Status: StatusOK, Detail: `platform "compose" manifest lints (project fixture-agent, 2 env keys set)`},
				{Name: "spec-gate", Status: StatusWarn, Detail: "pinned image ghcr.io/tankdonut/agent-base:2026.09.05 not local — skipped the real-image spec gate (run `agentctl deploy` once or pull it)"},
			}},
		},
		{
			name: "engineless host fails the platform construct",
			look: nil,
			want: doctorReport{Meta: meta, Checks: []CheckResult{
				specOK, tagOK, secretsOK, litellmWarn,
				{Name: "platform", Status: StatusFail, Detail: `platform "compose": no container engine found — install podman or docker`},
				{Name: "spec-gate", Status: StatusWarn, Detail: "no compose engine — skipped the real-image spec gate"},
			}, Failed: true},
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

// TestDoctorTarget pins the --target era-crossings preview: crossings
// from the era table render as era/<id> lines carrying their own
// severity (a fail entry on an applicable spec FAILs the report),
// zero-crossing and downgrade targets get their own lines, malformed
// targets fail closed before any check runs, and --json carries
// meta.target.
func TestDoctorTarget(t *testing.T) {
	retag := func(t *testing.T, root, newTag string) {
		t.Helper()
		path := filepath.Join(root, "agent", "Dockerfile")
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
		addLitellmSidecar(t, root)
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
	path := filepath.Join(root, "agent", "Dockerfile")
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
// the static compose /backups mount line, and the volume probes over
// the Platform port (warm marker, migration-boot shape, fresh volume,
// unreachable instance).
func TestDoctorBackupReadiness(t *testing.T) {
	backupsCompose := `name: fixture-agent
services:
  agent:
    build: {context: ., dockerfile: agent/Dockerfile}
    volumes:
      - agent-data:/home/node/.openclaw
      - agent-backups:/backups
volumes:
  agent-data:
  agent-backups:
`
	probeKey := func(command string) string {
		return "podman compose -f compose.yml exec -T agent sh -c " + command
	}
	withBackups := func(t *testing.T) string {
		t.Helper()
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		if err := os.WriteFile(filepath.Join(root, "compose.yml"), []byte(backupsCompose), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("static mount present", func(t *testing.T) {
		root := withBackups(t)
		stubbedRunner(t, "podman")
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "ok    named volume mounted at /backups — migration archives survive container replacement") {
			t.Errorf("output lacks the mount ok line:\n%s", out)
		}
	})

	t.Run("static mount absent warns", func(t *testing.T) {
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		stubbedRunner(t, "podman")
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "warn  no volume mounted at /backups") {
			t.Errorf("output lacks the mount warn:\n%s", out)
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
