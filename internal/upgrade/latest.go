package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// LatestURL is GitHub's API for the newest published (non-draft, non-prerelease) release.
const LatestURL = "https://api.github.com/repos/team-expanse/expanse/releases/latest"

// FetchLatest returns the newest release's version.
func FetchLatest(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(rel.Tag, "v")
	if !versionRE.MatchString(v) {
		return "", fmt.Errorf("newest release tag %q is not a version", rel.Tag)
	}
	return v, nil
}
