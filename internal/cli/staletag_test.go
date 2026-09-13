package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestMain serves a fake registry for the whole package so doctor's
// --target freshness check never touches the network in tests; tests
// that need specific registries override the endpoint vars themselves
// and restore them via t.Cleanup.
func TestMain(m *testing.M) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"fake"}`)
		case "/v2/tankdonut/agent-base/tags/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"tags":["2026.08.22","2026.09.05","2026.09.12","2026.09.12.1"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldToken, oldTags := ghcrTokenURL, ghcrTagsURL
	ghcrTokenURL = srv.URL + "/token"
	ghcrTagsURL = srv.URL + "/v2/tankdonut/agent-base/tags/list"
	code := m.Run()
	ghcrTokenURL, ghcrTagsURL = oldToken, oldTags
	os.Exit(code)
}

// fakeRegistry serves a token endpoint and a tags list; non-200
// statuses configure failure paths.
func fakeRegistry(t *testing.T, tokenStatus, tagsStatus int, tagsJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if tokenStatus != http.StatusOK {
				w.WriteHeader(tokenStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"fake"}`)
		case "/v2/tankdonut/agent-base/tags/list":
			if tagsStatus != http.StatusOK {
				w.WriteHeader(tagsStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, tagsJSON)
		default:
			http.NotFound(w, r)
		}
	}))
}

// pointAt retargets the freshness endpoints at srv for one test.
func pointAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldToken, oldTags := ghcrTokenURL, ghcrTagsURL
	ghcrTokenURL = srv.URL + "/token"
	ghcrTagsURL = srv.URL + "/v2/tankdonut/agent-base/tags/list"
	t.Cleanup(func() {
		ghcrTokenURL, ghcrTagsURL = oldToken, oldTags
	})
}

func TestNewestPublishedTag(t *testing.T) {
	t.Run("orders by day then run suffix", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusOK, http.StatusOK, `{"tags":["2026.09.12","2026.08.22","2026.09.12.1","2026.09.05"]}`)
		defer srv.Close()
		pointAt(t, srv)
		got, err := newestPublishedTag(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != "2026.09.12.1" {
			t.Fatalf("newest = %q, want 2026.09.12.1", got)
		}
	})

	t.Run("skips non-date tags", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusOK, http.StatusOK, `{"tags":["latest","sha-deadbeef","2026.09.05"]}`)
		defer srv.Close()
		pointAt(t, srv)
		got, err := newestPublishedTag(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != "2026.09.05" {
			t.Fatalf("newest = %q, want 2026.09.05", got)
		}
	})

	t.Run("token endpoint failure errors", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusInternalServerError, http.StatusOK, `{"tags":[]}`)
		defer srv.Close()
		pointAt(t, srv)
		if _, err := newestPublishedTag(context.Background()); err == nil || !strings.Contains(err.Error(), "token endpoint") {
			t.Fatalf("err = %v, want token endpoint error", err)
		}
	})

	t.Run("tags endpoint failure errors", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusOK, http.StatusForbidden, `{"tags":[]}`)
		defer srv.Close()
		pointAt(t, srv)
		if _, err := newestPublishedTag(context.Background()); err == nil || !strings.Contains(err.Error(), "tags endpoint") {
			t.Fatalf("err = %v, want tags endpoint error", err)
		}
	})

	t.Run("no date-shaped tags errors", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusOK, http.StatusOK, `{"tags":["latest"]}`)
		defer srv.Close()
		pointAt(t, srv)
		if _, err := newestPublishedTag(context.Background()); err == nil || !strings.Contains(err.Error(), "no date-shaped tags") {
			t.Fatalf("err = %v, want no-date-tags error", err)
		}
	})

	t.Run("unreachable registry errors", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusOK, http.StatusOK, `{"tags":["2026.09.05"]}`)
		srv.Close()
		pointAt(t, srv)
		if _, err := newestPublishedTag(context.Background()); err == nil {
			t.Fatal("expected a connection error for a closed registry")
		}
	})
}

// TestDoctorTagFreshness pins the advisory freshness lines under
// --target: stale target warns with the delta, the newest target is
// ok, an unpublished target warns, and an unreachable registry warns
// without failing the report.
func TestDoctorTagFreshness(t *testing.T) {
	t.Run("target older than newest warns with the delta", func(t *testing.T) {
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		r := stubbedRunner(t, "podman")
		r.runOutputOK = true
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.05")
		if !strings.Contains(out, "warn  newer image 2026.09.12.1 is published (target 2026.09.05)") {
			t.Errorf("output lacks the stale-target warn:\n%s", out)
		}
	})

	t.Run("target at the newest publishes ok", func(t *testing.T) {
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		r := stubbedRunner(t, "podman")
		r.runOutputOK = true
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "ok    target 2026.09.12.1 is the newest published tag") {
			t.Errorf("output lacks the newest-ok line:\n%s", out)
		}
	})

	t.Run("unpublished target warns", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusOK, http.StatusOK, `{"tags":["2026.09.05"]}`)
		defer srv.Close()
		pointAt(t, srv)
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		r := stubbedRunner(t, "podman")
		r.runOutputOK = true
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "warn  target 2026.09.12.1 is not published (newest is 2026.09.05)") {
			t.Errorf("output lacks the unpublished warn:\n%s", out)
		}
	})

	t.Run("registry unreachable warns and keeps the report", func(t *testing.T) {
		srv := fakeRegistry(t, http.StatusOK, http.StatusOK, `{"tags":["2026.09.05"]}`)
		srv.Close()
		pointAt(t, srv)
		root := fixtureProject(t)
		pinFixture(t, root, "2026.09.12")
		r := stubbedRunner(t, "podman")
		r.runOutputOK = true
		out, _ := execIn(t, root, "doctor", "--target", "2026.09.12.1")
		if !strings.Contains(out, "warn  registry unreachable — skipped the freshness check") {
			t.Errorf("output lacks the unreachable warn:\n%s", out)
		}
	})
}
