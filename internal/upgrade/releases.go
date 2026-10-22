package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

// ReleasesURL is GitHub's API for the most recent releases.
const ReleasesURL = "https://api.github.com/repos/team-expanse/expanse/releases?per_page=30"

// FetchReleases returns the published (non-draft, non-prerelease) release versions, newest first.
func FetchReleases(ctx context.Context, client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	var rels []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rels); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rels {
		if v := strings.TrimPrefix(r.Tag, "v"); !r.Draft && !r.Prerelease && versionRE.MatchString(v) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no published release", url)
	}
	slices.SortFunc(out, func(a, b string) int { return semver.Compare("v"+b, "v"+a) })
	return out, nil
}

// Newer reports whether release a is newer than b; an empty or unknown version is oldest.
func Newer(a, b string) bool { return semver.Compare("v"+a, "v"+b) > 0 }
