package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/state"
)

// dismissReq is a request to DELETE /notifications/threads/{id}. We enqueue
// one ID per `d` press; a single worker goroutine drains it sequentially so
// we never fire concurrent writes against GitHub (which trips secondary
// rate-limits and used to silently lose some requests).
type dismissReq struct {
	ID string
}

// dismissOutcome reports the result of one queued dismissal back to
// the main loop so it can update state.LastError, un-record failed
// entries from DismissedNotifs, and trigger a re-render.
type dismissOutcome struct {
	ID  string
	Err error
}

// dismissWorker drains the queue one at a time and posts outcomes back
// to the main loop. Sequential by design.
func dismissWorker(ctx context.Context, client *ghclient.Client, in <-chan dismissReq, out chan<- dismissOutcome) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-in:
			if !ok {
				return
			}
			err := dismissWithRetry(ctx, client, req.ID)
			select {
			case out <- dismissOutcome{ID: req.ID, Err: err}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// dismissMaxAttempts caps the total tries for one dismissal: one initial
// attempt plus this-many retries. Three is enough to ride out the brief
// 5xx blips GitHub sometimes serves without dragging a single 'd' press
// out into seconds of perceived hang.
const dismissMaxAttempts = 4

// dismissWithRetry tries one DELETE up to dismissMaxAttempts times.
// Retries on 5xx (transient server errors) and on rate-limit responses
// (honouring Retry-After). 4xx and other errors return immediately.
// Cancelled via ctx during a backoff sleep.
func dismissWithRetry(ctx context.Context, client *ghclient.Client, id string) error {
	var lastErr error
	for attempt := 0; attempt < dismissMaxAttempts; attempt++ {
		err := notifs.DismissAll(client, []string{id})
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetryDismiss(err) {
			return err
		}
		if attempt < dismissMaxAttempts-1 {
			wait := dismissBackoff(attempt, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return lastErr
}

func shouldRetryDismiss(err error) bool {
	if he, ok := ghclient.AsHTTPError(err); ok {
		return he.IsServerError()
	}
	if _, ok := ghclient.AsRateLimited(err); ok {
		return true
	}
	return false
}

// dismissBackoff returns the wait duration before the next retry.
// Honours Retry-After on rate-limit errors, otherwise exponential:
// 500ms, 1s, 2s.
func dismissBackoff(attempt int, err error) time.Duration {
	if rl, ok := ghclient.AsRateLimited(err); ok && rl.RetryAfter > 0 {
		return rl.RetryAfter
	}
	return time.Duration(500<<attempt) * time.Millisecond
}

func dismissFocused(queue chan<- dismissReq, st *state.State, cfg *watchConfig, f focusTarget) focusTarget {
	if f.Panel != "notifs" || f.ID == "" {
		return f
	}
	// Capture UpdatedAt before applyDismiss removes the row, so we can
	// suppress GitHub's stale-cache bounce-back on the next poll.
	var updatedAt time.Time
	for _, n := range st.LastNotifs {
		if n.ID == f.ID {
			updatedAt = n.UpdatedAt
			break
		}
	}
	newFocus, ok := applyDismiss(st, f.ID)
	if !ok {
		return f
	}
	st.RecordDismiss(f.ID, updatedAt)

	renderFromState(st, *cfg, false, newFocus)
	// Non-blocking enqueue. Buffer is generous; if it's somehow full,
	// drop and the user will see the notification reappear on next
	// poll (correctly reflecting GitHub state).
	select {
	case queue <- dismissReq{ID: f.ID}:
	default:
		// queue full; revert the bounce-back guard so the user sees
		// the notif return honestly instead of staying hidden.
		delete(st.DismissedNotifs, f.ID)
	}
	return newFocus
}

// dismissFocusedRun hides the focused workflow run from the table. Pure local
// operation -- no GitHub API call. The run is recorded in DismissedRuns so it
// stays hidden across subsequent polls until it ages out of the poll window.
func dismissFocusedRun(st *state.State, cfg *watchConfig, f focusTarget) focusTarget {
	if f.Panel != "runs" || f.ID == "" {
		return f
	}
	runID, err := strconv.ParseInt(f.ID, 10, 64)
	if err != nil {
		return f
	}
	newFocus, ok := applyRunDismiss(st, f.ID)
	if !ok {
		return f
	}
	st.DismissRun(runID)
	renderFromState(st, *cfg, false, newFocus)
	return newFocus
}

// markFocusedRead marks the focused notification read. No-op if the focused
// row is not in the notifications panel or is already read.
func markFocusedRead(client *ghclient.Client, st *state.State, cfg *watchConfig, f focusTarget) {
	if f.Panel != "notifs" || f.ID == "" {
		return
	}
	var matched string
	for i := range st.LastNotifs {
		if st.LastNotifs[i].ID == f.ID && st.LastNotifs[i].Unread {
			st.LastNotifs[i].Unread = false
			matched = f.ID
			break
		}
	}
	if matched == "" {
		return
	}
	renderFromState(st, *cfg, false, f)
	go func() {
		if err := notifs.MarkAllRead(client, []string{matched}); err != nil {
			fmt.Fprintf(os.Stderr, "mark read: %v\n", err)
		}
	}()
}

// markAllVisibleRead optimistically flips every unread notification in the
// cached snapshot to read, repaints, then fires PATCH calls in background.
func markAllVisibleRead(client *ghclient.Client, st *state.State, cfg *watchConfig, f focusTarget) {
	ids := make([]string, 0, len(st.LastNotifs))
	for i := range st.LastNotifs {
		if st.LastNotifs[i].Unread {
			ids = append(ids, st.LastNotifs[i].ID)
			st.LastNotifs[i].Unread = false
		}
	}
	if len(ids) == 0 {
		return
	}
	renderFromState(st, *cfg, false, f)
	go func() {
		if err := notifs.MarkAllRead(client, ids); err != nil {
			fmt.Fprintf(os.Stderr, "mark read: %v\n", err)
		}
	}()
}
