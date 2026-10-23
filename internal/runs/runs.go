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
	RunAttempt   int       `json:"run_attempt,omitempty"` // re-runs keep the ID and bump this
	Progress     *Progress `json:"progress,omitempty"`
}

// Progress summarises a run's jobs: how many have finished, which job and
// step are running now, and where a failed run broke.
type Progress struct {
	JobsDone     int    `json:"jobs_done"`
	JobsTotal    int    `json:"jobs_total"`
	Job          string `json:"job,omitempty"`           // earliest-started in-progress job
	Step         string `json:"step,omitempty"`          // its current (or last completed) step
	ParallelJobs int    `json:"parallel_jobs,omitempty"` // other jobs in progress alongside Job
	FailedJob    string `json:"failed_job,omitempty"`
	FailedStep   string `json:"failed_step,omitempty"`

	// Partial is set when the run has more jobs than one page returns: the
	// total is still right, but the current and failed job may be missed,
	// so the result is never cached as final.
	Partial bool `json:"-"`
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
	return isFailureConclusion(r.Conclusion)
}

func isFailureConclusion(c string) bool {
	switch c {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// IsBot reports whether the run was triggered by — or runs on behalf of — a
// known automated agent: Renovate, Dependabot, the Copilot coding agent
// (login "Copilot", e.g. "Addressing comment on PR #N"), or a Copilot code-review
// workflow. Detection covers all three of: actor login, head branch prefix,
// and workflow name.
func (r Run) IsBot() bool {
	switch strings.ToLower(r.ActorLogin) {
	case "renovate[bot]", "dependabot[bot]", "copilot", "copilot[bot]", "copilot-pull-request-reviewer[bot]":
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
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HeadBranch string `json:"head_branch"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		Actor      struct {
			Login string `json:"login"`
		} `json:"actor"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
		RunAttempt int       `json:"run_attempt"`
	} `json:"workflow_runs"`
}

const pollConcurrency = 8

// Poll fetches the most recent workflow runs for each repo in parallel,
// bounded to pollConcurrency simultaneous requests. The first rate-limit
// response is surfaced so the watch loop can honour Retry-After. Other
// per-repo failures are skipped, unless every repo failed (e.g. the network
// is down): then the first error is returned so the caller keeps its last
// good view instead of treating "no data" as "no runs".
func Poll(client *ghclient.Client, repos []discovery.Repo) ([]Run, error) {
	var (
		mu        sync.Mutex
		all       []Run
		fatal     error
		firstErr  error
		succeeded int
	)

	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(pollConcurrency)

	for _, r := range repos {
		r := r
		g.Go(func() error {
			path := fmt.Sprintf("repos/%s/%s/actions/runs?per_page=10", r.Owner, r.Name)
			var resp apiResp
			if err := client.Get(path, &resp); err != nil {
				mu.Lock()
				if _, ok := ghclient.AsRateLimited(err); ok && fatal == nil {
					fatal = err
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", r.FullName, err)
				}
				mu.Unlock()
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
					RunAttempt:   wr.RunAttempt,
				})
			}
			mu.Lock()
			all = append(all, batch...)
			succeeded++
			mu.Unlock()
			return nil
		})
	}

	_ = g.Wait()
	if fatal == nil && succeeded == 0 && firstErr != nil {
		fatal = firstErr
	}
	return all, fatal
}

type jobsResp struct {
	TotalCount int   `json:"total_count"`
	Jobs       []job `json:"jobs"`
}

type job struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	StartedAt  time.Time `json:"started_at"`
	Steps      []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

// FetchProgress fetches the latest attempt's jobs for one run and
// summarises them. repo is "owner/name".
func FetchProgress(client *ghclient.Client, repo string, id int64) (*Progress, error) {
	path := fmt.Sprintf("repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, id)
	var resp jobsResp
	if err := client.Get(path, &resp); err != nil {
		return nil, err
	}
	p := summarise(resp.Jobs)
	if resp.TotalCount > len(resp.Jobs) {
		p.JobsTotal = resp.TotalCount
		p.Partial = true
	}
	return p, nil
}

// FailedAt returns "job › step" for where a failed run broke, or "".
func (p *Progress) FailedAt() string {
	if p == nil {
		return ""
	}
	return JobStep(p.FailedJob, p.FailedStep)
}

// JobStep joins a job and step name as "job › step", or just the job.
func JobStep(job, step string) string {
	if step == "" {
		return job
	}
	return job + " › " + step
}

// stepLabel shortens the names GitHub generates for unnamed `uses:` steps,
// "Run owner/repo/path@ref", to the action's own name ("path"'s last
// segment). Steps with a real name, or unnamed `run:` scripts (which
// contain spaces), are returned unchanged.
func stepLabel(name string) string {
	for _, prefix := range []string{"Run ", "Post Run "} {
		ref, ok := strings.CutPrefix(name, prefix)
		if !ok || strings.ContainsAny(ref, " \t") {
			continue
		}
		action, _, hasRef := strings.Cut(ref, "@")
		if !hasRef || !strings.Contains(action, "/") {
			continue
		}
		label := action[strings.LastIndex(action, "/")+1:]
		if prefix == "Post Run " {
			label = "Post " + label
		}
		return label
	}
	return name
}

func summarise(jobs []job) *Progress {
	p := &Progress{JobsTotal: len(jobs)}
	var current *job
	for i := range jobs {
		j := &jobs[i]
		switch j.Status {
		case "completed":
			p.JobsDone++
			if p.FailedJob == "" && isFailureConclusion(j.Conclusion) {
				p.FailedJob = j.Name
				for _, s := range j.Steps {
					if isFailureConclusion(s.Conclusion) {
						p.FailedStep = stepLabel(s.Name)
						break
					}
				}
			}
		case "in_progress":
			if current == nil {
				current = j
			} else {
				p.ParallelJobs++
				if j.StartedAt.Before(current.StartedAt) {
					current = j
				}
			}
		}
	}
	if current != nil {
		p.Job = current.Name
		for _, s := range current.Steps {
			if s.Status == "in_progress" {
				p.Step = stepLabel(s.Name)
				break
			}
			if s.Status == "completed" {
				p.Step = stepLabel(s.Name)
			}
		}
	}
	return p
}
