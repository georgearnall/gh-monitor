package runs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/georgearnall/gh-monitor/internal/discovery"
	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"golang.org/x/sync/errgroup"
)

type Run struct {
	ID           int64     `json:"id"`
	Repo         string    `json:"repo"`
	WorkflowName string    `json:"workflow_name"`
	Branch       string    `json:"branch"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	URL          string    `json:"url"`
	ActorLogin   string    `json:"actor_login"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// IsActive reports whether the run is still in-flight (not yet conclusive).
func (r Run) IsActive() bool {
	switch r.Status {
	case "queued", "in_progress", "waiting", "pending", "requested":
		return true
	}
	return false
}

// IsFailure reports whether the run reached a notification-worthy failure state.
func (r Run) IsFailure() bool {
	switch r.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// IsBot reports whether the run was triggered by — or runs on behalf of — a
// known automated agent: Renovate, Dependabot, or a Copilot code-review
// workflow. Detection covers all three of: actor login, head branch prefix,
// and workflow name.
func (r Run) IsBot() bool {
	switch strings.ToLower(r.ActorLogin) {
	case "renovate[bot]", "dependabot[bot]", "copilot[bot]", "copilot-pull-request-reviewer[bot]":
		return true
	}
	if strings.HasPrefix(r.Branch, "renovate/") || strings.HasPrefix(r.Branch, "dependabot/") {
		return true
	}
	if strings.Contains(strings.ToLower(r.WorkflowName), "copilot") {
		return true
	}
	return false
}

type apiResp struct {
	WorkflowRuns []struct {
		ID         int64     `json:"id"`
		Name       string    `json:"name"`
		HeadBranch string    `json:"head_branch"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		HTMLURL    string    `json:"html_url"`
		Actor      struct {
			Login string `json:"login"`
		} `json:"actor"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"workflow_runs"`
}

const pollConcurrency = 8

// Poll fetches the most recent workflow runs for each repo in parallel,
// bounded to pollConcurrency simultaneous requests. The first rate-limit
// response is surfaced so the watch loop can honour Retry-After.
func Poll(client *ghclient.Client, repos []discovery.Repo) ([]Run, error) {
	var (
		mu    sync.Mutex
		all   []Run
		fatal error
	)

	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(pollConcurrency)

	for _, r := range repos {
		r := r
		g.Go(func() error {
			path := fmt.Sprintf("repos/%s/%s/actions/runs?per_page=10", r.Owner, r.Name)
			var resp apiResp
			if err := client.Get(path, &resp); err != nil {
				if _, ok := ghclient.AsRateLimited(err); ok {
					mu.Lock()
					if fatal == nil {
						fatal = err
					}
					mu.Unlock()
				}
				return nil
			}
			batch := make([]Run, 0, len(resp.WorkflowRuns))
			for _, wr := range resp.WorkflowRuns {
				batch = append(batch, Run{
					ID:           wr.ID,
					Repo:         r.FullName,
					WorkflowName: wr.Name,
					Branch:       wr.HeadBranch,
					Status:       wr.Status,
					Conclusion:   wr.Conclusion,
					URL:          wr.HTMLURL,
					ActorLogin:   wr.Actor.Login,
					CreatedAt:    wr.CreatedAt,
					UpdatedAt:    wr.UpdatedAt,
				})
			}
			mu.Lock()
			all = append(all, batch...)
			mu.Unlock()
			return nil
		})
	}

	_ = g.Wait()
	return all, fatal
}
