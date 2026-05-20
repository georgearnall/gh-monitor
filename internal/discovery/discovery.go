package discovery

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"golang.org/x/sync/errgroup"
)

type Repo struct {
	FullName string    `json:"full_name"`
	Owner    string    `json:"owner"`
	Name     string    `json:"name"`
	Activity time.Time `json:"activity"`
	HTMLURL  string    `json:"html_url"`
}

// Discover returns up to maxRepos active repositories for the authenticated
// user, deduped from /user/repos (recently pushed-to) and /search/issues
// (repos with open PRs by the user). Sorted by activity descending.
func Discover(client *ghclient.Client, maxRepos int) ([]Repo, error) {
	var pushed, pr, activity []Repo

	g, _ := errgroup.WithContext(context.Background())
	g.Go(func() error {
		out, err := fetchUserRepos(client)
		if err != nil {
			return fmt.Errorf("user repos: %w", err)
		}
		pushed = out
		return nil
	})
	g.Go(func() error {
		out, err := fetchOpenPRRepos(client)
		if err != nil {
			return fmt.Errorf("open-PR repos: %w", err)
		}
		pr = out
		return nil
	})
	g.Go(func() error {
		out, err := fetchActivityRepos(client)
		if err != nil {
			return fmt.Errorf("activity repos: %w", err)
		}
		activity = out
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	merged := mergeRepos(mergeRepos(pushed, pr), activity)
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

// fetchActivityRepos picks up repos the user has recently pushed to, opened or
// merged PRs in, or released. Catches the case where access is granted via team
// membership (so /user/repos with affiliation=owner,collaborator misses them)
// but the user actually contributes — e.g. deployment repos triggered via
// workflow_dispatch.
func fetchActivityRepos(client *ghclient.Client) ([]Repo, error) {
	login, err := client.Viewer()
	if err != nil {
		return nil, err
	}

	var events []struct {
		Type string `json:"type"`
		Repo struct {
			Name string `json:"name"`
		} `json:"repo"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := client.Get("users/"+login+"/events?per_page=100", &events); err != nil {
		return nil, err
	}

	latest := make(map[string]time.Time, len(events))
	for _, e := range events {
		switch e.Type {
		case "PushEvent", "PullRequestEvent", "ReleaseEvent", "CreateEvent":
		default:
			continue
		}
		if cur, ok := latest[e.Repo.Name]; !ok || e.CreatedAt.After(cur) {
			latest[e.Repo.Name] = e.CreatedAt
		}
	}

	out := make([]Repo, 0, len(latest))
	for name, ts := range latest {
		parts := strings.SplitN(name, "/", 2)
		if len(parts) != 2 {
			continue
		}
		out = append(out, Repo{
			FullName: name,
			Owner:    parts[0],
			Name:     parts[1],
			Activity: ts,
			HTMLURL:  "https://github.com/" + name,
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
