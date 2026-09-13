package cli

import (
	"encoding/json"
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
