package prs

import (
	"sort"
	"time"

	"github.com/georgearnall/gha-monitor/internal/ghclient"
)

type PR struct {
	Repo           string    `json:"repo"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	HeadBranch     string    `json:"head_branch"`
	URL            string    `json:"url"`
	UpdatedAt      time.Time `json:"updated_at"`
	State          string    `json:"state"` // SUCCESS|FAILURE|ERROR|PENDING|EXPECTED|""
	Passing        int       `json:"passing"`
	Failing        int       `json:"failing"`
	Pending        int       `json:"pending"`
	Total          int       `json:"total"`
	ReviewDecision string    `json:"review_decision"` // APPROVED|CHANGES_REQUESTED|REVIEW_REQUIRED|""
	ReviewCount    int       `json:"review_count"`
	CommentCount   int       `json:"comment_count"`
}

// IsFailing reports whether any check in the rollup is in a failure state.
func (p PR) IsFailing() bool {
	switch p.State {
	case "FAILURE", "ERROR":
		return true
	}
	return false
}

// IsPending reports whether any check is still running.
func (p PR) IsPending() bool {
	switch p.State {
	case "PENDING", "EXPECTED":
		return true
	}
	return false
}

// IsPassing reports whether all checks (if any) succeeded.
func (p PR) IsPassing() bool {
	return p.State == "SUCCESS"
}

const query = `
{
  viewer {
    pullRequests(states: OPEN, first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number
        title
        url
        isDraft
        updatedAt
        headRefName
        reviewDecision
        reviews(first: 1) { totalCount }
        comments(first: 1) { totalCount }
        repository { nameWithOwner }
        commits(last: 1) {
          nodes {
            commit {
              statusCheckRollup {
                state
                contexts(first: 100) {
                  totalCount
                  nodes {
                    __typename
                    ... on CheckRun { conclusion status }
                    ... on StatusContext { state }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

type response struct {
	Viewer struct {
		PullRequests struct {
			Nodes []prNode `json:"nodes"`
		} `json:"pullRequests"`
	} `json:"viewer"`
}

type prNode struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	IsDraft        bool      `json:"isDraft"`
	UpdatedAt      time.Time `json:"updatedAt"`
	HeadRefName    string    `json:"headRefName"`
	ReviewDecision string    `json:"reviewDecision"`
	Reviews        struct {
		TotalCount int `json:"totalCount"`
	} `json:"reviews"`
	Comments struct {
		TotalCount int `json:"totalCount"`
	} `json:"comments"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						TotalCount int           `json:"totalCount"`
						Nodes      []contextNode `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type contextNode struct {
	Typename string `json:"__typename"`
	// CheckRun fields
	Conclusion string `json:"conclusion,omitempty"`
	Status     string `json:"status,omitempty"`
	// StatusContext field
	State string `json:"state,omitempty"`
}

// Poll returns the authenticated user's open, non-draft pull requests with
// aggregated status-check counts. PRs whose UpdatedAt is before `since` are
// dropped; a zero `since` disables the filter and returns everything.
func Poll(client *ghclient.Client, since time.Time) ([]PR, error) {
	var resp response
	if err := client.GraphQL(query, nil, &resp); err != nil {
		return nil, err
	}

	out := make([]PR, 0, len(resp.Viewer.PullRequests.Nodes))
	for _, n := range resp.Viewer.PullRequests.Nodes {
		if n.IsDraft {
			continue
		}
		if !since.IsZero() && n.UpdatedAt.Before(since) {
			continue
		}
		pr := PR{
			Repo:           n.Repository.NameWithOwner,
			Number:         n.Number,
			Title:          n.Title,
			HeadBranch:     n.HeadRefName,
			URL:            n.URL,
			UpdatedAt:      n.UpdatedAt,
			ReviewDecision: n.ReviewDecision,
			ReviewCount:    n.Reviews.TotalCount,
			CommentCount:   n.Comments.TotalCount,
		}
		if len(n.Commits.Nodes) > 0 {
			if r := n.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil {
				pr.State = r.State
				pr.Total = r.Contexts.TotalCount
				for _, ctx := range r.Contexts.Nodes {
					bucket(ctx, &pr)
				}
			}
		}
		out = append(out, pr)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IsFailing() != out[j].IsFailing() {
			return out[i].IsFailing()
		}
		if out[i].IsPending() != out[j].IsPending() {
			return out[i].IsPending()
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})

	return out, nil
}

func bucket(c contextNode, pr *PR) {
	switch c.Typename {
	case "CheckRun":
		if c.Status != "COMPLETED" {
			pr.Pending++
			return
		}
		switch c.Conclusion {
		case "SUCCESS":
			pr.Passing++
		case "FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "CANCELLED":
			pr.Failing++
		default:
			// NEUTRAL, SKIPPED, STALE — count as passing for rollup purposes,
			// since they don't block a merge.
			pr.Passing++
		}
	case "StatusContext":
		switch c.State {
		case "SUCCESS":
			pr.Passing++
		case "FAILURE", "ERROR":
			pr.Failing++
		case "PENDING", "EXPECTED":
			pr.Pending++
		}
	}
}
