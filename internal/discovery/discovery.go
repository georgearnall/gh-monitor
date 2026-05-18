package discovery

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/georgearnall/gha-monitor/internal/ghclient"
)

type Repo struct {
	FullName string
	Owner    string
	Name     string
	Activity time.Time
	HTMLURL  string
}

// Discover returns up to maxRepos active repositories for the authenticated
// user, deduped from /user/repos (recently pushed-to) and /search/issues
// (repos with open PRs by the user). Sorted by activity descending.
func Discover(client *ghclient.Client, maxRepos int) ([]Repo, error) {
	pushed, err := fetchUserRepos(client)
	if err != nil {
		return nil, fmt.Errorf("user repos: %w", err)
	}

	pr, err := fetchOpenPRRepos(client)
	if err != nil {
		return nil, fmt.Errorf("open-PR repos: %w", err)
	}

	merged := mergeRepos(pushed, pr)
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Activity.After(merged[j].Activity)
	})

	if maxRepos > 0 && len(merged) > maxRepos {
		merged = merged[:maxRepos]
	}
	return merged, nil
}

type userReposItem struct {
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Name     string    `json:"name"`
	PushedAt time.Time `json:"pushed_at"`
	HTMLURL  string    `json:"html_url"`
}

func fetchUserRepos(client *ghclient.Client) ([]Repo, error) {
	var items []userReposItem
	path := "user/repos?sort=pushed&affiliation=owner,collaborator&per_page=30"
	if err := client.Get(path, &items); err != nil {
		return nil, err
	}
	out := make([]Repo, 0, len(items))
	for _, it := range items {
		out = append(out, Repo{
			FullName: it.FullName,
			Owner:    it.Owner.Login,
			Name:     it.Name,
			Activity: it.PushedAt,
			HTMLURL:  it.HTMLURL,
		})
	}
	return out, nil
}

type searchIssuesResp struct {
	Items []struct {
		RepositoryURL string    `json:"repository_url"`
		UpdatedAt     time.Time `json:"updated_at"`
	} `json:"items"`
}

func fetchOpenPRRepos(client *ghclient.Client) ([]Repo, error) {
	q := url.QueryEscape("author:@me is:pr is:open")
	path := "search/issues?q=" + q + "&per_page=30"
	var resp searchIssuesResp
	if err := client.Get(path, &resp); err != nil {
		return nil, err
	}
	seen := map[string]time.Time{}
	for _, it := range resp.Items {
		full := strings.TrimPrefix(it.RepositoryURL, "https://api.github.com/repos/")
		if full == "" || full == it.RepositoryURL {
			continue
		}
		if cur, ok := seen[full]; !ok || it.UpdatedAt.After(cur) {
			seen[full] = it.UpdatedAt
		}
	}
	out := make([]Repo, 0, len(seen))
	for full, ts := range seen {
		parts := strings.SplitN(full, "/", 2)
		if len(parts) != 2 {
			continue
		}
		out = append(out, Repo{
			FullName: full,
			Owner:    parts[0],
			Name:     parts[1],
			Activity: ts,
			HTMLURL:  "https://github.com/" + full,
		})
	}
	return out, nil
}

func mergeRepos(a, b []Repo) []Repo {
	byName := make(map[string]Repo, len(a)+len(b))
	for _, r := range a {
		byName[r.FullName] = r
	}
	for _, r := range b {
		if cur, ok := byName[r.FullName]; ok {
			if r.Activity.After(cur.Activity) {
				cur.Activity = r.Activity
			}
			byName[r.FullName] = cur
			continue
		}
		byName[r.FullName] = r
	}
	out := make([]Repo, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	return out
}
