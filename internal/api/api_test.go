package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
)

// stubPlatform satisfies the Platform port; Deploy records the agent
// root and fails when the fixture planted a failure marker.
type stubPlatform struct {
	deployed map[string]bool
}

func (p *stubPlatform) Name() string { return "stub" }
func (p *stubPlatform) Capabilities() platform.Capabilities {
	return platform.Capabilities{Exec: true, StopStart: true}
}
func (p *stubPlatform) Check(root string, d *platform.Deployment) error { return nil }
func (p *stubPlatform) Deploy(ctx context.Context, r process.Runner, root string, d *platform.Deployment, opts platform.DeployOptions, out platform.Output) error {
	if _, err := os.Stat(filepath.Join(root, "FAIL_MARKER")); err == nil {
		return stubFail(root)
	}
	p.deployed[root] = true
	out.Printf("deployed %s\n", d.Project)
	return nil
}
func (p *stubPlatform) Status(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	return nil
}
func (p *stubPlatform) Logs(ctx context.Context, r process.Runner, root string, d *platform.Deployment, follow bool, out platform.Output) error {
	return nil
}
func (p *stubPlatform) Mcp(ctx context.Context, r process.Runner, root string, d *platform.Deployment, args []string, out platform.Output) error {
	return nil
}
func (p *stubPlatform) Stop(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	return nil
}
func (p *stubPlatform) Start(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	return nil
}
func (p *stubPlatform) Destroy(ctx context.Context, r process.Runner, root string, d *platform.Deployment, destroyData bool, out platform.Output) error {
	return nil
}
func (p *stubPlatform) Backup(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	return nil
}
func (p *stubPlatform) Probe(ctx context.Context, r process.Runner, root string, d *platform.Deployment, command string) (string, error) {
	return "", nil
}

type stubFail string

func (e stubFail) Error() string { return "stub failure: " + string(e) }

// twoAgentDeps builds a server over a two-agent fixture. The stub
// platform is shared so tests can assert on deployments.
func twoAgentDeps(t *testing.T) (Deps, *stubPlatform) {
	t.Helper()
	m, err := fleet.LoadManifest(writeFleetFixture(t, "agents:\n  grow: {}\n  trade: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubPlatform{deployed: map[string]bool{}}
	deps := Deps{
		Manifest:  m,
		NewRunner: func() process.Runner { return nil },
		PlatformFor: func(agent string) (string, platform.Platform, platform.Deployment, error) {
			entry := m.Agents[agent]
			d, err := platform.Derive(entry.Dir)
			if err != nil {
				return "", nil, platform.Deployment{}, err
			}
			return entry.Dir, stub, d, nil
		},
		Version: "test",
		Token:   "serve-token-canary",
	}
	return deps, stub
}

// writeFleetFixture materializes a fleet repo: manifest plus the
// per-agent contract files Derive needs (spec.json, Dockerfile, .env).
func writeFleetFixture(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fleet.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"grow", "trade"} {
		dir := filepath.Join(root, "agents", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"spec.json":    `{"agent":{"name":"` + name + `"}}`,
			"Dockerfile":   "FROM ghcr.io/tankdonut/agent-base:2026.09.05\n",
			".env.example": "#ZAI_API_KEY=\n",
			".env":         "ZAI_API_KEY=canary-env-value\n",
		}
		for rel, content := range files {
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func authedRequest(method, url, body string) *http.Request {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer serve-token-canary")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestAuthNegatives(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()

	paths := []string{"/api/v1/roster", "/api/v1/status", "/api/v1/jobs", "/api/v1/approvals", "/api/v1/events"}
	for _, path := range paths {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/roster", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", resp.StatusCode)
	}
	resp2, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200 without auth", resp2.StatusCode)
	}
}

func TestDeployJobLifecycleAndIsolation(t *testing.T) {
	deps, stub := twoAgentDeps(t)
	// trade's agent dir carries the failure marker: deploy --all must
	// still deploy grow, then report the job as failed.
	if err := os.WriteFile(filepath.Join(deps.Manifest.Agents["trade"].Dir, "FAIL_MARKER"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/deploy", `{"all":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy = %d, want 202", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if started.JobID == "" {
		t.Fatal("no job id in the 202 body")
	}

	var job struct {
		State string   `json:"state"`
		Log   []string `json:"log"`
		Error string   `json:"error"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := client.Do(authedRequest(http.MethodGet, srv.URL+"/api/v1/jobs/"+started.JobID, ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if job.State == StateDone || job.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != StateFailed {
		t.Fatalf("job state = %s, want failed (log %v)", job.State, job.Log)
	}
	if len(stub.deployed) != 1 {
		t.Fatalf("deployed roots = %v, want grow only (failure isolates)", stub.deployed)
	}
	for root := range stub.deployed {
		if !strings.Contains(root, "grow") {
			t.Errorf("wrong root deployed: %s", root)
		}
	}
	// Unknown job → 404.
	if r, _ := client.Do(authedRequest(http.MethodGet, srv.URL+"/api/v1/jobs/nope", "")); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown job = %d, want 404", r.StatusCode)
	}
}

func TestDeployScopeContract(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	resp, _ := client.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/deploy", `{"agent":"ghost"}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = client.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/deploy", `{"agent":"grow","all":true}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("agent+all = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	// Two-agent roster: no scope at all is refused (never implicit).
	resp, _ = client.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/deploy", `{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unscoped = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCanaryThroughEveryHandler locks the secrets contract over the
// API surface: the canary planted in agent/.env and the serve token
// must never appear in ANY handler response.
func TestCanaryThroughEveryHandler(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	probes := []struct {
		method, path, body string
	}{
		{"GET", "/healthz", ""},
		{"GET", "/api/v1/roster", ""},
		{"GET", "/api/v1/status", ""},
		{"GET", "/api/v1/jobs", ""},
		{"GET", "/api/v1/jobs/missing", ""},
		{"GET", "/api/v1/approvals", ""},
		{"GET", "/api/v1/approvals?agent=grow", ""},
		{"POST", "/api/v1/deploy", `{"agent":"grow"}`},
		{"POST", "/api/v1/approvals/resolve", `{"id":"x","decision":"approve"}`},
	}
	// Give the deploy job a beat so its log exists before the sweep.
	sweep := func() {
		for _, p := range probes {
			resp, err := client.Do(authedRequest(p.method, srv.URL+p.path, p.body))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			for _, secret := range []string{"canary-env-value", "serve-token-canary"} {
				if strings.Contains(string(raw), secret) {
					t.Errorf("%s %s response leaks %q:\n%s", p.method, p.path, secret, raw)
				}
			}
		}
	}
	sweep()
	time.Sleep(300 * time.Millisecond)
	sweep()
}

func TestSSECarriesJobTransitions(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := authedRequest(http.MethodGet, srv.URL+"/api/v1/events", "")
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Fire a deploy while the stream is open; the job transitions must
	// arrive on the stream.
	go func() {
		r, _ := http.DefaultClient.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/deploy", `{"agent":"grow"}`))
		if r != nil {
			r.Body.Close()
		}
	}()

	sawRunning, sawDone := false, false
	sc := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(4 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		if !strings.HasPrefix(sc.Text(), "data: ") {
			continue
		}
		var evt struct {
			Type  string `json:"type"`
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &evt); err != nil {
			continue
		}
		if evt.Type != "job" {
			continue
		}
		switch evt.State {
		case StateRunning:
			sawRunning = true
		case StateDone, StateFailed:
			sawDone = true
		}
		if sawRunning && sawDone {
			return
		}
	}
	t.Errorf("SSE stream missed job transitions (running=%v done=%v)", sawRunning, sawDone)
}
