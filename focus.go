package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
	"github.com/georgearnall/gh-monitor/internal/state"
	"github.com/georgearnall/gh-monitor/internal/ui"
)

// focusTarget identifies one focused row across any of the three panels.
// Zero value means "no focus" (empty state).
type focusTarget struct {
	Panel string // "notifs" | "prs" | "runs"
	ID    string // panel-specific identifier
}

// prKey returns the stable identifier we use to track a PR row across
// refreshes: "owner/repo#number".
func prKey(p prs.PR) string {
	return fmt.Sprintf("%s#%d", p.Repo, p.Number)
}

// runKey returns the stable identifier for a workflow run row.
func runKey(r runs.Run) string {
	return strconv.FormatInt(r.ID, 10)
}

// cursorTargets materialises the flat ordered list of every focusable row
// across all three panels, in the same order they're rendered. Critically,
// it applies the same VisibleNotifs / VisibleRows filters the UI uses so
// the cursor never lands on a row that isn't on screen (otherwise arrow
// keys feel "sticky" as they walk through invisible items).
func cursorTargets(st *state.State) []focusTarget {
	notifRows := ui.VisibleNotifs(st.LastNotifs)
	runRows := ui.VisibleRows(st.LastView, st.ViewerLogin)
	out := make([]focusTarget, 0, len(notifRows)+len(st.LastPRs)+len(runRows))
	for _, n := range notifRows {
		out = append(out, focusTarget{"notifs", n.ID})
	}
	for _, p := range st.LastPRs {
		out = append(out, focusTarget{"prs", prKey(p)})
	}
	for _, r := range runRows {
		out = append(out, focusTarget{"runs", runKey(r)})
	}
	return out
}

// pickFocus returns current if it still appears in the cursor target list,
// otherwise falls back to the first unread notification (if visible),
// otherwise the first target row overall. Returns the zero value when
// there's nothing focusable.
func pickFocus(st *state.State, current focusTarget) focusTarget {
	targets := cursorTargets(st)
	if current.Panel != "" {
		for _, t := range targets {
			if t == current {
				return current
			}
		}
	}
	for _, n := range ui.VisibleNotifs(st.LastNotifs) {
		if n.Unread {
			return focusTarget{"notifs", n.ID}
		}
	}
	if len(targets) > 0 {
		return targets[0]
	}
	return focusTarget{}
}

// moveFocus advances the cursor by delta rows across the flat target list,
// clamped at both ends.
func moveFocus(st *state.State, current focusTarget, delta int) focusTarget {
	targets := cursorTargets(st)
	if len(targets) == 0 {
		return focusTarget{}
	}
	idx := -1
	for i, t := range targets {
		if t == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		return targets[0]
	}
	next := idx + delta
	if next < 0 {
		next = 0
	}
	if next >= len(targets) {
		next = len(targets) - 1
	}
	return targets[next]
}

// applyDismiss removes the notification with id from local state and
// returns the focus that should take its place: the next row in the
// notifications list (clamped at the end), or pickFocus's fallback when
// the notifications panel becomes empty. The bool reports whether the
// notification was actually present. Pure: no I/O, no goroutines.
func applyDismiss(st *state.State, id string) (focusTarget, bool) {
	idx := -1
	for i, n := range st.LastNotifs {
		if n.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return focusTarget{}, false
	}
	st.LastNotifs = append(st.LastNotifs[:idx], st.LastNotifs[idx+1:]...)
	if len(st.LastNotifs) == 0 {
		return pickFocus(st, focusTarget{}), true
	}
	ni := idx
	if ni >= len(st.LastNotifs) {
		ni = len(st.LastNotifs) - 1
	}
	return focusTarget{"notifs", st.LastNotifs[ni].ID}, true
}

// openFocused launches the URL of whatever row the cursor is on in the
// user's default browser. Non-blocking.
func openFocused(st *state.State, f focusTarget) {
	url := focusedURL(st, f)
	if url == "" {
		return
	}
	if err := exec.Command("open", url).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
	}
}

func focusedURL(st *state.State, f focusTarget) string {
	switch f.Panel {
	case "notifs":
		for _, n := range st.LastNotifs {
			if n.ID == f.ID {
				return n.URL
			}
		}
	case "prs":
		for _, p := range st.LastPRs {
			if prKey(p) == f.ID {
				return p.URL
			}
		}
	case "runs":
		for _, r := range st.LastView {
			if runKey(r) == f.ID {
				return r.URL
			}
		}
	}
	return ""
}
