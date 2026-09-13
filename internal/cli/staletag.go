package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Registry endpoints for the tag-freshness advisory — GHCR's anonymous
// pull flow. Vars (not consts) so tests point them at a fake registry.
var (
	ghcrTokenURL = "https://ghcr.io/token?scope=repository:tankdonut/agent-base:pull&service=ghcr.io"
	ghcrTagsURL  = "https://ghcr.io/v2/tankdonut/agent-base/tags/list"
)

// newestPublishedTag asks the registry for the newest published base
// tag — anonymous token, then tags/list, ordered by the same (day,
// run) tuple as tagAfter; non-date tags are skipped. Every failure
// returns an error; callers degrade to a warn, never a failure.
func newestPublishedTag(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}

	var tok struct {
		Token string `json:"token"`
	}
	if err := getJSON(ctx, client, ghcrTokenURL, "", &tok); err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}

	var list struct {
		Tags []string `json:"tags"`
	}
	if err := getJSON(ctx, client, ghcrTagsURL, tok.Token, &list); err != nil {
		return "", fmt.Errorf("tags endpoint: %w", err)
	}
	newest := ""
	for _, tag := range list.Tags {
		if tagDate(tag).IsZero() {
			continue
		}
		if newest == "" || tagAfter(tag, newest) {
			newest = tag
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no date-shaped tags published")
	}
	return newest, nil
}

// getJSON fetches url into into — Bearer-authed when bearer is set.
func getJSON(ctx context.Context, client *http.Client, url, bearer string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
