package notifs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/georgearnall/gha-monitor/internal/ghclient"
	"golang.org/x/sync/errgroup"
)

// PRState is the lifecycle state of a pull request.
type PRState string

const (
	PRStateOpen   PRState = "OPEN"
	PRStateClosed PRState = "CLOSED"
	PRStateMerged PRState = "MERGED"
	PRStateDraft  PRState = "DRAFT"
)

// Notification is one GitHub notification, narrowed to PR threads we want to
// surface (mentions, review requests, replies on threads we're in, and
// activity on PRs we authored or were assigned to).
type Notification struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	PRNumber  int       `json:"pr_number"`
	Title     string    `json:"title"`
	Reason    string    `json:"reason"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updated_at"`
	Unread    bool      `json:"unread"`
	// PRState is the linked PR's state. Empty when not yet fetched (best-effort enrichment).
	PRState PRState `json:"pr_state,omitempty"`
}

// readWindow is how long a read notification stays visible before dropping.
const readWindow = 7 * 24 * time.Hour

// allowedReasons is the set of GitHub notification reasons that justify
// surfacing in the feed. Everything else (subscribed, ci_activity, push,
// state_change, security_*, etc.) is filtered out.
var allowedReasons = map[string]bool{
	"mention":          true,
	"team_mention":     true,
	"review_requested": true,
	"comment":          true,
	"author":           true,
	"assign":           true,
}

type apiNotification struct {
	ID         string    `json:"id"`
	Unread     bool      `json:"unread"`
	Reason     string    `json:"reason"`
	UpdatedAt  time.Time `json:"updated_at"`
	Subject    struct {
		Title            string `json:"title"`
		URL              string `json:"url"`
		LatestCommentURL string `json:"latest_comment_url"`
		Type             string `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Poll fetches GitHub notifications and returns only the PR-related entries
// matching the allowed reason set, sorted unread-first then most-recent.
// Read entries older than 7 days are dropped.
func Poll(client *ghclient.Client) ([]Notification, error) {
	var resp []apiNotification
	if err := client.Get("notifications?all=true&per_page=50", &resp); err != nil {
		return nil, err
	}

	now := time.Now()
	out := make([]Notification, 0, len(resp))
	for _, n := range resp {
		if n.Subject.Type != "PullRequest" {
			continue
		}
		if !allowedReasons[n.Reason] {
			continue
		}
		if !n.Unread && now.Sub(n.UpdatedAt) > readWindow {
			continue
		}
		num := parsePRNumber(n.Subject.URL)
		if num == 0 {
			continue
		}
		out = append(out, Notification{
			ID:        n.ID,
			Repo:      n.Repository.FullName,
			PRNumber:  num,
			Title:     n.Subject.Title,
			Reason:    n.Reason,
			URL:       buildHTMLURL(n, num),
			UpdatedAt: n.UpdatedAt,
			Unread:    n.Unread,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Unread != out[j].Unread {
			return out[i].Unread
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// FetchPRStates batches PR state lookups into one GraphQL request keyed
// by per-notification alias (pr0, pr1, ...). Returns a map of
// notification.ID -> PRState. Best-effort: notifications whose repo/PR
// can't be resolved are simply absent from the result.
func FetchPRStates(client *ghclient.Client, ns []Notification) (map[string]PRState, error) {
	if len(ns) == 0 {
		return nil, nil
	}
	var query strings.Builder
	query.WriteString("{\n")
	type indexed struct {
		alias string
		notif Notification
	}
	var indices []indexed
	for i, n := range ns {
		if n.Repo == "" || n.PRNumber == 0 {
			continue
		}
		slash := strings.IndexByte(n.Repo, '/')
		if slash < 0 {
			continue
		}
		owner := n.Repo[:slash]
		name := n.Repo[slash+1:]
		alias := fmt.Sprintf("pr%d", i)
		indices = append(indices, indexed{alias, n})
		fmt.Fprintf(&query, "  %s: repository(owner: %q, name: %q) {\n", alias, owner, name)
		fmt.Fprintf(&query, "    pullRequest(number: %d) { state isDraft }\n", n.PRNumber)
		query.WriteString("  }\n")
	}
	query.WriteString("}")
	if len(indices) == 0 {
		return nil, nil
	}

	type prNode struct {
		State   string `json:"state"`
		IsDraft bool   `json:"isDraft"`
	}
	type repoNode struct {
		PullRequest *prNode `json:"pullRequest"`
	}
	var data map[string]*repoNode
	if err := client.GraphQLBestEffort(query.String(), nil, &data); err != nil {
		return nil, err
	}

	out := make(map[string]PRState, len(indices))
	for _, ix := range indices {
		repo := data[ix.alias]
		if repo == nil || repo.PullRequest == nil {
			continue
		}
		state := PRState(repo.PullRequest.State)
		if repo.PullRequest.IsDraft {
			state = PRStateDraft
		}
		out[ix.notif.ID] = state
	}
	return out, nil
}

// MarkAllRead fires PATCH /notifications/threads/{id} for each given thread
// ID, in parallel, bounded to 8 concurrent requests. Returns the first error
// encountered; remaining requests still complete.
func MarkAllRead(client *ghclient.Client, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(8)
	var (
		mu       sync.Mutex
		firstErr error
	)
	for _, id := range ids {
		id := id
		g.Go(func() error {
			if err := client.Patch("notifications/threads/"+id, nil); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return firstErr
}

// DismissAll fires DELETE /notifications/threads/{id} for each given
// thread ID, in parallel, bounded to 8 concurrent requests. This is
// GitHub's "mark as done" operation: the thread is removed from the
// inbox entirely and won't appear on subsequent polls.
func DismissAll(client *ghclient.Client, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	g, _ := errgroup.WithContext(context.Background())
	g.SetLimit(8)
	var (
		mu       sync.Mutex
		firstErr error
	)
	for _, id := range ids {
		id := id
		g.Go(func() error {
			if err := client.Delete("notifications/threads/" + id); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return firstErr
}

// buildHTMLURL produces a clickable github.com URL. When the reason is a new
// reply on a thread, it anchors to the specific comment so the link jumps
// straight to it.
func buildHTMLURL(n apiNotification, prNumber int) string {
	plain := "https://github.com/" + n.Repository.FullName + "/pull/" + strconv.Itoa(prNumber)
	if n.Reason != "comment" || n.Subject.LatestCommentURL == "" {
		return plain
	}
	if anchored := apiCommentURLtoHTML(n.Subject.LatestCommentURL, plain); anchored != "" {
		return anchored
	}
	return plain
}

// apiCommentURLtoHTML converts an api.github.com comment URL into the
// HTML PR URL with the right fragment, so clicking jumps to the comment.
//
//	.../issues/comments/9999 -> {pr}#issuecomment-9999
//	.../pulls/comments/9999  -> {pr}#discussion_r9999
//
// Returns "" if the URL doesn't match a known shape; the caller falls back.
func apiCommentURLtoHTML(api, prURL string) string {
	switch {
	case strings.Contains(api, "/issues/comments/"):
		id := api[strings.LastIndex(api, "/")+1:]
		if _, err := strconv.Atoi(id); err != nil {
			return ""
		}
		return prURL + "#issuecomment-" + id
	case strings.Contains(api, "/pulls/comments/"):
		id := api[strings.LastIndex(api, "/")+1:]
		if _, err := strconv.Atoi(id); err != nil {
			return ""
		}
		return prURL + "#discussion_r" + id
	}
	return ""
}

// parsePRNumber pulls the trailing integer out of an API subject URL such as
//
//	https://api.github.com/repos/o/r/pulls/123
//
// Returns 0 if no parseable number is found.
func parsePRNumber(api string) int {
	if api == "" {
		return 0
	}
	tail := api[strings.LastIndex(api, "/")+1:]
	n, err := strconv.Atoi(tail)
	if err != nil {
		return 0
	}
	return n
}
