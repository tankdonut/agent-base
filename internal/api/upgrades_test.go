package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpgradesPreviewEndpoint(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	deps.UpgradesPreview = func(agent, target string) (any, error) {
		return map[string]any{
			"agent": agent, "current_tag": "2026.09.05", "target": target,
			"downgrade": target < "2026.09.05",
			"crossings": []map[string]string{},
		}, nil
	}
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(authedRequest(http.MethodGet, srv.URL+"/api/v1/upgrades?agent=grow&target=2026.09.14", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Agent   string `json:"agent"`
		Preview struct {
			CurrentTag string `json:"current_tag"`
			Target     string `json:"target"`
			Downgrade  bool   `json:"downgrade"`
		} `json:"preview"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Agent != "grow" || body.Preview.CurrentTag != "2026.09.05" || body.Preview.Target != "2026.09.14" {
		t.Errorf("preview body = %+v", body)
	}

	// Missing target is a 400; unimplemented callback is a 501.
	resp, _ = client.Do(authedRequest(http.MethodGet, srv.URL+"/api/v1/upgrades?agent=grow", ""))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no target = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	noPreview := deps
	noPreview.UpgradesPreview = nil
	srv2 := httptest.NewServer(New(noPreview).mux)
	defer srv2.Close()
	resp, _ = client.Do(authedRequest(http.MethodGet, srv2.URL+"/api/v1/upgrades?agent=grow&target=2026.09.14", ""))
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("nil callback = %d, want 501", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestApplyUpgradeRetagsAndConverges(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	dockerfile := filepath.Join(deps.Manifest.Agents["grow"].Dir, "Dockerfile")
	before, _ := os.ReadFile(dockerfile)
	if !strings.Contains(string(before), ":2026.09.05") {
		t.Fatalf("fixture Dockerfile lacks the base pin:\n%s", before)
	}

	resp, err := client.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/upgrades/apply", `{"agent":"grow","tag":"2026.09.14"}`))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply = %d, want 202", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	after, _ := os.ReadFile(dockerfile)
	if !strings.Contains(string(after), ":2026.09.14") {
		t.Errorf("Dockerfile not retagged:\n%s", after)
	}

	// The converge job completes through the normal machinery.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := client.Do(authedRequest(http.MethodGet, srv.URL+"/api/v1/jobs/"+started.JobID, ""))
		if err != nil {
			t.Fatal(err)
		}
		var job struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job.State == StateDone || job.State == StateFailed {
			if job.State != StateDone {
				t.Fatalf("upgrade job = %s", job.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upgrade job never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestApplyUpgradeRejectsBadTags(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}

	for _, tag := range []string{"latest", "2026", "2026.09", "upgrade", "2026.09.14-rc1", ""} {
		body := `{"agent":"grow","tag":"` + tag + `"}`
		resp, err := client.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/upgrades/apply", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("tag %q = %d, want 400", tag, resp.StatusCode)
		}
	}
	// Unknown agent still 404s after the tag check.
	resp, _ := client.Do(authedRequest(http.MethodPost, srv.URL+"/api/v1/upgrades/apply", `{"agent":"ghost","tag":"2026.09.14"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent = %d, want 404", resp.StatusCode)
	}
}

func TestConsoleServedUnauthenticated(t *testing.T) {
	deps, _ := twoAgentDeps(t)
	srv := httptest.NewServer(New(deps).mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "agentctl fleet") {
		t.Errorf("index content unexpected: %.80s", buf[:n])
	}
	for _, asset := range []string{"/app.js", "/style.css"} {
		r, err := http.Get(srv.URL + asset)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", asset, r.StatusCode)
		}
	}
}
