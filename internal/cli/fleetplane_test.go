package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tankdonut/agent-base/internal/fleet"
)

// sharedPlaneRepo builds a two-agent fleet on a shared plane and fills
// plane/.env (observability off keeps the stub surface small).
func sharedPlaneRepo(t *testing.T) string {
	t.Helper()
	body := "plane:\n  enabled: true\n  name: t-plane\n  litellm: shared\n  observability: false\nagents:\n  grow: {}\n  trade:\n    litellm: sidecar\n"
	root := makeFleetRepo(t, body, []string{"grow", "trade"}, nil)
	if err := os.MkdirAll(filepath.Join(root, fleet.PlaneDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, fleet.PlaneDir, ".env"),
		[]byte("LITELLM_MASTER_KEY=sk-master-canary\nPOSTGRES_PASSWORD=canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, root)
	t.Cleanup(restore)
	return root
}

func TestFleetPlaneUpRunsComposeInPlaneDir(t *testing.T) {
	root := sharedPlaneRepo(t)
	r := stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "plane", "up")
	if code != 0 {
		t.Fatalf("fleet plane up failed:\n%s", out)
	}
	if !strings.Contains(out, "bringing up plane stack") {
		t.Errorf("missing plane preamble:\n%s", out)
	}
	upSeen := false
	for _, call := range r.calls {
		if len(call) >= 5 && call[1] == "compose" && call[4] == "up" {
			upSeen = true
		}
	}
	if !upSeen {
		t.Errorf("no compose up recorded: %v", r.calls)
	}
	// The plane artifacts materialized next to the manifest; the lite
	// plane renders no socket mount (that is the observability stack).
	for _, rel := range []string{fleet.PlaneDir + "/compose.yml", fleet.PlaneDir + "/litellm/config.yaml"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s not rendered: %v", rel, err)
		}
	}
	compose, _ := os.ReadFile(filepath.Join(root, fleet.PlaneDir, "compose.yml"))
	if strings.Contains(string(compose), "PLANE_ENGINE_SOCK") || strings.Contains(string(compose), "alloy") {
		t.Errorf("observability=false must not render the alloy/socket surface:\n%s", compose)
	}
	if !strings.Contains(string(compose), "litellm-db:") {
		t.Errorf("shared plane must render the litellm-db service:\n%s", compose)
	}
}

func TestFleetPlaneDownVolumesArgv(t *testing.T) {
	sharedPlaneRepo(t)
	r := stubbedRunner(t, "podman")

	if code, out := runFleet(t, "fleet", "plane", "down", "--volumes"); code != 0 {
		t.Fatalf("fleet plane down failed:\n%s", out)
	}
	sawVolumes := false
	for _, call := range r.calls {
		if len(call) >= 6 && call[4] == "down" && call[5] == "--volumes" {
			sawVolumes = true
		}
	}
	if !sawVolumes {
		t.Errorf("down --volumes argv not recorded: %v", r.calls)
	}
}

func TestFleetPlaneDisabledRefuses(t *testing.T) {
	root := makeFleetRepo(t, "agents:\n  grow: {}\n", []string{"grow"}, nil)
	restore := chdir(t, root)
	t.Cleanup(restore)
	stubbedRunner(t, "podman")

	if code, out := runFleet(t, "fleet", "plane", "up"); code != 1 || !strings.Contains(out, "plane is disabled") {
		t.Fatalf("code = %d out = %q", code, out)
	}
}

func TestFleetRenderMaterializesEverything(t *testing.T) {
	root := sharedPlaneRepo(t)
	stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "render")
	if code != 0 {
		t.Fatalf("fleet render failed:\n%s", out)
	}
	for _, want := range []string{
		"agents/grow/compose.yml",
		"plane/compose.yml",
		"render complete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q:\n%s", want, out)
		}
	}
	// Shared agent envelope joins the plane network; the sidecar agent
	// does not.
	growEnv, _ := os.ReadFile(filepath.Join(root, "agents", "grow", "compose.yml"))
	if !strings.Contains(string(growEnv), "t-plane-net") {
		t.Error("shared agent envelope missing the plane network join")
	}
}

func TestFleetKeyMintsAndNeverPrints(t *testing.T) {
	root := sharedPlaneRepo(t)

	var gotAuth, gotAlias string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth = req.Header.Get("Authorization")
		var body struct {
			KeyAlias string `json:"key_alias"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		gotAlias = body.KeyAlias
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"sk-virtual-canary","token_id":"tok-1"}`))
	}))
	defer srv.Close()
	_, port, _ := strings.Cut(srv.URL, "//")
	port = strings.Split(port, ":")[1]
	planeEnv := filepath.Join(root, fleet.PlaneDir, ".env")
	_ = os.WriteFile(planeEnv, []byte("LITELLM_MASTER_KEY=sk-master-canary\nPOSTGRES_PASSWORD=canary\nPLANE_LITELLM_PORT="+port+"\n"), 0o600)

	code, out := runFleet(t, "fleet", "key", "grow")
	if code != 0 {
		t.Fatalf("fleet key failed:\n%s", out)
	}
	if gotAuth != "Bearer sk-master-canary" {
		t.Errorf("proxy call not master-authenticated: %q", gotAuth)
	}
	if gotAlias != "grow" {
		t.Errorf("key_alias = %q, want grow", gotAlias)
	}
	// The canary values must reach .env but never the CLI output.
	agentEnv, _ := os.ReadFile(filepath.Join(root, "agents", "grow", ".env"))
	if !strings.Contains(string(agentEnv), "LITELLM_API_KEY=sk-virtual-canary") {
		t.Errorf("minted key not written to agent .env:\n%s", agentEnv)
	}
	if strings.Contains(out, "sk-virtual-canary") || strings.Contains(out, "sk-master-canary") {
		t.Errorf("key values leaked into output:\n%s", out)
	}
	if !strings.Contains(out, "value not shown") {
		t.Errorf("output must state the value is not shown:\n%s", out)
	}
}

func TestFleetKeyUnreachableProxyNamesPlaneUp(t *testing.T) {
	sharedPlaneRepo(t)
	// Port 1 refuses connections immediately.
	_ = os.WriteFile(filepath.Join(root(t), fleet.PlaneDir, ".env"),
		[]byte("LITELLM_MASTER_KEY=sk-master-canary\nPOSTGRES_PASSWORD=canary\nPLANE_LITELLM_PORT=1\n"), 0o600)

	if code, out := runFleet(t, "fleet", "key", "grow"); code != 1 || !strings.Contains(out, "fleet plane up") {
		t.Fatalf("code = %d out = %q", code, out)
	}
}

func TestFleetAddSharedSkipsLocalLitellm(t *testing.T) {
	root := sharedPlaneRepo(t)
	stubbedRunner(t, "podman")

	code, out := runFleet(t, "fleet", "add", "scout", "--telegram=false")
	if code != 0 {
		t.Fatalf("fleet add failed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "scout", "litellm")); !os.IsNotExist(err) {
		t.Error("shared-plane fleet add must not emit the agent-local litellm/ tree")
	}
	if !strings.Contains(out, "note:") {
		t.Errorf("plane advisory missing:\n%s", out)
	}
}

// root is the chdir'd repo root for tests that mutate files after
// sharedPlaneRepo returned.
func root(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
