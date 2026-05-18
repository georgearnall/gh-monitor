package runs

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/georgearnall/gha-monitor/internal/discovery"
)

type Run struct {
	ID           int64
	Repo         string
	WorkflowName string
	Branch       string
	Status       string
	Conclusion   string
	URL          string
	CreatedAt    time.Time
	UpdatedAt    time.Time
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

type apiResp struct {
	WorkflowRuns []struct {
		ID         int64     `json:"id"`
		Name       string    `json:"name"`
		HeadBranch string    `json:"head_branch"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		HTMLURL    string    `json:"html_url"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
	} `json:"workflow_runs"`
}

// Poll fetches the most recent workflow runs for each repo, serialised with
// random jitter to avoid tripping GitHub's secondary rate limits.
func Poll(client *api.RESTClient, repos []discovery.Repo) ([]Run, error) {
	var all []Run
	for i, r := range repos {
		if i > 0 {
			jitter := 200 + rand.IntN(300)
			time.Sleep(time.Duration(jitter) * time.Millisecond)
		}
		path := fmt.Sprintf("repos/%s/%s/actions/runs?per_page=10", r.Owner, r.Name)
		var resp apiResp
		if err := client.Get(path, &resp); err != nil {
			// Skip a single repo's failure; don't abort the whole pass.
			continue
		}
		for _, wr := range resp.WorkflowRuns {
			all = append(all, Run{
				ID:           wr.ID,
				Repo:         r.FullName,
				WorkflowName: wr.Name,
				Branch:       wr.HeadBranch,
				Status:       wr.Status,
				Conclusion:   wr.Conclusion,
				URL:          wr.HTMLURL,
				CreatedAt:    wr.CreatedAt,
				UpdatedAt:    wr.UpdatedAt,
			})
		}
	}
	return all, nil
}
