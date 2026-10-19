// Package sshkeys expands gh:<user> entries in SSH key lists into that GitHub user's public keys.
package sshkeys

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// DefaultGitHubURL serves <user>.keys for every GitHub user.
const DefaultGitHubURL = "https://github.com"

var githubUserRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// Expand replaces each gh:<user> in keys with that user's keys from base ("" is GitHub).
func Expand(ctx context.Context, base string, keys []string) ([]string, error) {
	var out []string
	for _, k := range keys {
		user, ok := strings.CutPrefix(k, "gh:")
		if !ok {
			out = append(out, k)
			continue
		}
		fetched, err := GitHub(ctx, base, user)
		if err != nil {
			return nil, err
		}
		out = append(out, fetched...)
	}
	return out, nil
}

// GitHub fetches a GitHub user's public SSH keys from <base>/<user>.keys ("" is GitHub).
func GitHub(ctx context.Context, base, user string) ([]string, error) {
	if !githubUserRE.MatchString(user) {
		return nil, fmt.Errorf("gh:%s: not a GitHub user name", user)
	}
	if base == "" {
		base = DefaultGitHubURL
	}
	// user matched githubUserRE, so it cannot leave the GitHub host's path.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+user+".keys", nil) //nolint:gosec // see above
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // see above
	if err != nil {
		return nil, fmt.Errorf("gh:%s: cannot reach GitHub: %w", user, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gh:%s: GitHub answered %s", user, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("gh:%s: %w", user, err)
	}
	var keys []string
	for _, line := range strings.Split(string(body), "\n") {
		if k := strings.TrimSpace(line); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("gh:%s has no public SSH keys on GitHub", user)
	}
	return keys, nil
}
