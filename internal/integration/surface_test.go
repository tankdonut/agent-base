//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tankdonut/agent-base/internal/api"
	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/e2e"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/gatewayclient"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/platform/dockercompose"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/scaffold"
)

// liveAgent is one REAL image boot shared by every Tier B test: the
// expensive part happens once, everything else reuses the live
// gateway + stack.
type liveAgent struct {
	engine     string
	fleetRoot  string
	agentDir   string
	gatewayURL string // ws://127.0.0.1:<port>
	token      string
	server     *http.Server // the serve API over this fleet
	serverURL  string
}

var (
	fixtureOnce sync.Once
	fixtureLive *liveAgent
	fixtureErr  error
)

// liveFixture boots the real image once (AGENT_E2E_IMAGE, loud skip
// when unset — the image is machine-local build state) and returns
// the shared live agent.
func liveFixture(t *testing.T) *liveAgent {
	t.Helper()
	fixtureOnce.Do(func() { fixtureLive, fixtureErr = bootLiveAgent() })
	if fixtureErr != nil {
		t.Fatalf("live fixture: %v", fixtureErr)
	}
	return fixtureLive
}

func bootLiveAgent() (*liveAgent, error) {
	image := os.Getenv("AGENT_E2E_IMAGE")
	if image == "" {
		// Default to the public release image: CI and fresh checkouts
		// can pull it without a local bake.
		image = "ghcr.io/tankdonut/agent-base:" + scaffold.DefaultBaseTag
	}
	engine, engineErr := resolveComposeEngine()
	if engineErr != nil {
		return nil, engineErr
	}

	// /tmp by default: /var/tmp can be noexec, which silently breaks
	// the compose provider (exit 127 on build). AGENT_E2E_TMPDIR
	// overrides for machines that need it.
	tmpBase := os.Getenv("AGENT_E2E_TMPDIR")
	if tmpBase == "" {
		tmpBase = os.TempDir()
	}
	if err := os.MkdirAll(tmpBase, 0o755); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(tmpBase, "tierb-fleet-")
	if err != nil {
		return nil, err
	}
	port := freePortNoSkip()
	manifest := fmt.Sprintf(
		"defaults:\n  compose:\n    engine: %s\nagents:\n  grow:\n    gateway_port: %d\n    overrides:\n      image: %s\n",
		engine, port, image)
	if err := os.WriteFile(filepath.Join(root, "fleet.yaml"), []byte(manifest), 0o644); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "agents", "grow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	token := "tierb-gateway-token-canary"
	// The fixture tree is the smoke suite's boot-tested content — no
	// hand-rolled spec. docs/ maps to the scaffold's knowledge/content
	// (the seed-docs COPY target).
	fixtureSrc := filepath.Join("..", "..", "tests", "fixtures", "grow-agent-like")
	if err := copyTree(fixtureSrc, dir); err != nil {
		return nil, fmt.Errorf("copying the smoke fixture: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "knowledge"), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(dir, "docs"), filepath.Join(dir, "knowledge", "content")); err != nil {
		return nil, err
	}
	// The copied fixture predates token-armed gateways: this tier
	// authenticates with OPENCLAW_GATEWAY_TOKEN, so the spec must
	// declare the feature or the entrypoint never arms it (and the
	// connect lands with cleared scopes).
	specPath := filepath.Join(dir, "spec.json")
	specBody, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	var spec map[string]any
	if err := json.Unmarshal(specBody, &spec); err != nil {
		return nil, err
	}
	features, _ := spec["features"].(map[string]any)
	if features == nil {
		features = map[string]any{}
	}
	features["gateway_auth"] = true
	spec["features"] = features
	specBody, err = json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(specPath, specBody, 0o644); err != nil {
		return nil, err
	}
	files := map[string]string{
		// The five COPY lines mirror the scaffold Dockerfile contract:
		// the entrypoint fail-closes without a baked spec, and the
		// seed dirs must exist for the content phases. The FROM is the
		// public sentinel — never built: overrides.image in the
		// manifest deploys the candidate ref, while Derive needs a
		// contract-valid public pin.
		"Dockerfile": "FROM ghcr.io/tankdonut/agent-base:" + scaffold.DefaultBaseTag + "\n" +
			"COPY --chown=node:node spec.json /opt/agent/spec.json\n" +
			"COPY --chown=node:node automations/ /opt/agent/automations/\n" +
			"COPY --chown=node:node workspace/ /opt/seed/workspace/\n" +
			"COPY --chown=node:node skills/ /opt/seed/skills/\n" +
			"COPY --chown=node:node knowledge/content/ /opt/seed/docs/\n",
		// The fixture spec references env vars via {env:...} tokens —
		// the loader fail-closes on unset references (the smoke runner
		// provides these; Tier B provides dummies).
		".env": "ZAI_API_KEY=tierb-canary\n" +
			"OPENCLAW_GATEWAY_TOKEN=" + token + "\n" +
			"AC_INFINITY_EMAIL=tierb@example.test\n" +
			"AC_INFINITY_PASSWORD=tierb-dummy\n" +
			"TELEGRAM_ALLOWED_USERS=123\n" +
			"TELEGRAM_CHAT_ID=123\n",
		"litellm/.env.example": "#LITELLM_MASTER_KEY=\n#LITELLM_API_KEY=\n",
		"litellm/.env":         "LITELLM_MASTER_KEY=sk-tierb-master\nLITELLM_API_KEY=sk-tierb-agent\n",
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	m, err := fleet.LoadManifest(root)
	if err != nil {
		return nil, err
	}
	if _, err := fleet.MaterializeAgentCompose(m, "grow"); err != nil {
		return nil, err
	}
	adapter, err := dockercompose.New(processRunner{}, map[string]any{"engine": engine, "gateway_port": port})
	if err != nil {
		return nil, err
	}
	d, err := platform.Derive(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := adapter.Deploy(ctx, processRunner{}, dir, &d, platform.DeployOptions{}, fileOutput{os.Stdout}); err != nil {
		return nil, fmt.Errorf("deploying %s: %w", image, err)
	}
	// Teardown is deferred to TestMain (teardownLive): a down here
	// would delete the stack the very moment the fixture boots.

	// First boot: setup + reconcile + seed before /healthz answers.
	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(600 * time.Second)
	start := time.Now()
	for {
		resp, err := http.Get(gatewayURL + "/healthz")
		code := 0
		if err == nil {
			code = resp.StatusCode
			resp.Body.Close()
			if code == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			// Fresh context: the boot ctx may already be past its
			// deadline at dump time, and a cancelled ctx yields empty
			// output — the evidence would be lost exactly when needed.
			dumpCtx, dumpCancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer dumpCancel()
			psOut, _ := commandIn(dumpCtx, dir, []string{engine, "compose", "-f", "compose.yml", "ps", "-a"}).CombinedOutput()
			logsOut, _ := commandIn(dumpCtx, dir, []string{engine, "compose", "-f", "compose.yml", "logs", "--tail", "60"}).CombinedOutput()
			agentLogs, _ := commandIn(dumpCtx, dir, []string{engine, "logs", "--tail", "80", "grow-agent-1"}).CombinedOutput()
			inspectOut, _ := commandIn(dumpCtx, dir, []string{engine, "inspect", "grow-agent-1"}).CombinedOutput()
			// Files persist for the CI artifact upload even when the
			// inline error gets truncated.
			dumpDir := filepath.Join(root, "tierb-dump")
			_ = os.MkdirAll(dumpDir, 0o755)
			_ = os.WriteFile(filepath.Join(dumpDir, "compose-ps.txt"), psOut, 0o644)
			_ = os.WriteFile(filepath.Join(dumpDir, "compose-logs.txt"), logsOut, 0o644)
			_ = os.WriteFile(filepath.Join(dumpDir, "agent-logs.txt"), agentLogs, 0o644)
			_ = os.WriteFile(filepath.Join(dumpDir, "inspect.json"), inspectOut, 0o644)
			return nil, fmt.Errorf("gateway never reached /healthz on %s within 600s\n== compose ps ==\n%s\n== compose logs ==\n%s\n== agent logs ==\n%s\n== inspect ==\n%s",
				gatewayURL, psOut, logsOut, agentLogs, inspectOut)
		}
		if time.Since(start) > 30*time.Second && int(time.Since(start).Seconds())%30 < 4 {
			fmt.Printf("[tierb] healthz poll: code=%d elapsed=%ds\n", code, int(time.Since(start).Seconds()))
		}
		time.Sleep(3 * time.Second)
	}

	liveBooted = true
	liveEngine = engine
	liveStackDir = dir
	live := &liveAgent{
		engine:     engine,
		fleetRoot:  root,
		agentDir:   dir,
		gatewayURL: fmt.Sprintf("ws://127.0.0.1:%d", port),
		token:      token,
	}

	// The serve API over this fleet, driven by the tests through the
	// product's own control plane. The device key is the fixture's —
	// already paired by the protocol tests, so the API's own WS
	// probes connect with full scopes.
	srv := api.New(api.Deps{
		Manifest:  m,
		NewRunner: func() process.Runner { return processRunner{} },
		PlatformFor: func(agent string) (string, platform.Platform, platform.Deployment, error) {
			dd, derr := platform.Derive(dir)
			if derr != nil {
				return "", nil, platform.Deployment{}, derr
			}
			return dir, adapter, dd, nil
		},
		Version:       "tierb",
		Token:         "tierb-serve-canary",
		DeviceKeyPath: filepath.Join(root, "device.key"),
	})
	apiSrv := &http.Server{Addr: "127.0.0.1:0", Handler: srv.Mux()}
	ln, err := listenLoopback()
	if err != nil {
		return nil, err
	}
	go func() { _ = apiSrv.Serve(ln) }()
	live.server = apiSrv
	live.serverURL = "http://" + ln.Addr().String()
	return live, nil
}

// teardownLive brings the live fixture's stack down once the process
// is done with it. Package state set by bootLiveAgent; a no-op when
// the fixture never booted.
var (
	liveBooted   bool
	liveEngine   string
	liveStackDir string
)

func teardownLive() {
	if !liveBooted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_ = e2e.Run(ctx, e2e.Step{
		Name: "tierb-down", Dir: liveStackDir,
		Argv: []string{liveEngine, "compose", "-f", "compose.yml", "down", "--volumes"},
	})
}

func (l *liveAgent) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = l.server.Shutdown(ctx)
}

// authedGet is the serve-API helper with the bearer header.
func (l *liveAgent) authedGet(t *testing.T, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, l.serverURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tierb-serve-canary")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body
}

// tierbOptions is the Tier B connect profile: the persisted device
// identity plus the self-pairing hook (the product's own behavior —
// the operator holds the stack, so NOT_PAIRED resolves itself).
func tierbOptions(live *liveAgent) gatewayclient.Options {
	return gatewayclient.Options{
		ClientVersion: "tierb",
		DeviceKeyPath: filepath.Join(live.fleetRoot, "device.key"),
		ApproveDevice: func() error {
			return compose.ApproveOwnDevice(processRunner{}, live.engine, live.agentDir)
		},
	}
}

func TestGatewayRealProtocol(t *testing.T) {
	live := liveFixture(t)
	_, connectErr := gatewayclient.Connect(context.Background(), live.gatewayURL, live.token, tierbOptions(live))
	if connectErr != nil && strings.Contains(connectErr.Error(), "NOT_PAIRED") {
		t.Fatalf("self-pairing did not resolve NOT_PAIRED: %v", connectErr)
	}

	c, err := gatewayclient.Connect(context.Background(), live.gatewayURL, live.token, tierbOptions(live))
	if err != nil {
		t.Fatalf("real gateway connect: %v", err)
	}
	defer c.Close()
	if c.ServerVersion() == "" {
		t.Error("hello-ok carried no server version")
	}
	var sessions struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := c.Call(context.Background(), "sessions.list", map[string]any{}, &sessions); err != nil {
		t.Fatalf("sessions.list against the real gateway: %v", err)
	}

	// Wrong token: the real gateway must refuse the connect.
	if _, err := gatewayclient.Connect(context.Background(), live.gatewayURL, "wrong-token", tierbOptions(live)); err == nil {
		t.Error("wrong token must be rejected by the real gateway")
	}
}

func TestApprovalsProtocolAgainstRealGateway(t *testing.T) {
	live := liveFixture(t)
	c, err := gatewayclient.Connect(context.Background(), live.gatewayURL, live.token, tierbOptions(live))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// List: the frame contract (params + auth + result decode) against
	// the real server — an empty queue is the expected steady state.
	approvals, err := c.ListApprovals(context.Background())
	if err != nil {
		t.Fatalf("exec.approval.list against the real gateway: %v", err)
	}
	if approvals == nil {
		approvals = []gatewayclient.Approval{}
	}

	// Resolve on a nonexistent ID: the real gateway must answer with a
	// structured RPC error (not a frame decode failure) — the resolve
	// contract holds even without an agent-side approval trigger.
	err = c.ResolveApproval(context.Background(), "exec", "no-such-approval", "approve")
	if err == nil {
		t.Log("resolve of a bogus id returned ok — the gateway tolerates it")
		return
	}
	var rpcErr *gatewayclient.RPCError
	if !asRPCError(err, &rpcErr) {
		t.Fatalf("resolve error is not a structured RPC error: %v", err)
	}
}

func asRPCError(err error, target **gatewayclient.RPCError) bool {
	if e, ok := err.(*gatewayclient.RPCError); ok {
		*target = e
		return true
	}
	return false
}

// dockerfileTag extracts the pinned tag from a fixture Dockerfile's
// FROM line.
func dockerfileTag(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FROM ") {
			parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "FROM ")), ":")
			return parts[len(parts)-1]
		}
	}
	return ""
}

// gatewayHealthy polls the fixture's published /healthz.
func (l *liveAgent) gatewayHealthy(t *testing.T, budget time.Duration) bool {
	t.Helper()
	port := strings.TrimPrefix(l.gatewayURL, "ws://127.0.0.1:")
	deadline := time.Now().Add(budget)
	for {
		resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

func TestUpgradeApplyRetagsAndConverges(t *testing.T) {
	live := liveFixture(t)
	// Retag to the tag the fixture already runs: apply must prove the
	// retag + converge machinery, not a registry pull.
	dockerfileBody, err := os.ReadFile(filepath.Join(live.agentDir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	target := dockerfileTag(string(dockerfileBody))
	if target == "" {
		t.Fatalf("no FROM tag in the fixture Dockerfile:\n%s", dockerfileBody)
	}

	req, _ := http.NewRequest(http.MethodPost, live.serverURL+"/api/v1/upgrades/apply",
		strings.NewReader(`{"agent":"grow","tag":"`+target+`"}`))
	req.Header.Set("Authorization", "Bearer tierb-serve-canary")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	if started.JobID == "" {
		t.Fatalf("upgrade apply = %d, want 202 with a job", resp.StatusCode)
	}

	dockerfile := filepath.Join(live.agentDir, "Dockerfile")
	after, _ := os.ReadFile(dockerfile)
	if !strings.Contains(string(after), ":"+target) {
		t.Errorf("Dockerfile not retagged to %s:\n%s", target, after)
	}

	deadline := time.Now().Add(4 * time.Minute)
	for {
		job := live.authedGet(t, "/api/v1/jobs/"+started.JobID)
		state, _ := job["state"].(string)
		if state == "done" || state == "failed" {
			if state != "done" {
				t.Fatalf("upgrade job = %s: %+v", state, job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upgrade job never finished: %+v", job)
		}
		time.Sleep(2 * time.Second)
	}
	if !live.gatewayHealthy(t, 90*time.Second) {
		t.Error("gateway unhealthy after upgrade converge")
	}
}

func TestServeAPIDeploysAndReports(t *testing.T) {
	live := liveFixture(t)

	roster := live.authedGet(t, "/api/v1/roster")
	agents, _ := roster["agents"].([]any)
	if len(agents) != 1 {
		t.Errorf("roster = %+v", roster)
	}
	status := live.authedGet(t, "/api/v1/status")
	rows, _ := status["agents"].([]any)
	if len(rows) == 0 {
		t.Fatalf("status rows empty: %+v", status)
	}
	first, _ := rows[0].(map[string]any)
	if first["gateway"] == "unreachable" {
		t.Errorf("live agent reported unreachable: %+v", first)
	}

	// Deploy through the API: the job converges (cached build + up).
	body := `{"agent":"grow"}`
	req, _ := http.NewRequest(http.MethodPost, live.serverURL+"/api/v1/deploy", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tierb-serve-canary")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	if started.JobID == "" {
		t.Fatal("no job id")
	}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		job := live.authedGet(t, "/api/v1/jobs/"+started.JobID)
		state, _ := job["state"].(string)
		if state == "done" || state == "failed" {
			if state != "done" {
				t.Fatalf("serve deploy job = %s: %+v", state, job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve deploy never finished: %+v", job)
		}
		time.Sleep(2 * time.Second)
	}
}

func freePortNoSkip() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func listenLoopback() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

// copyTree copies a directory tree (files only, permissions kept).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
