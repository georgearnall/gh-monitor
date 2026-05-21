package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
)

func TestPickFocus_NotifsFirstUnread(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{
		{ID: "1", Unread: false},
		{ID: "2", Unread: true},
		{ID: "3", Unread: true},
	}
	if got := pickFocus(st, focusTarget{}); got != (focusTarget{"notifs", "2"}) {
		t.Errorf("empty current should pick first unread; got %+v", got)
	}
}

func TestPickFocus_PreservesExisting(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{{ID: "1"}, {ID: "2"}}
	st.LastPRs = []prs.PR{{Repo: "a/b", Number: 7}}
	current := focusTarget{"prs", "a/b#7"}
	if got := pickFocus(st, current); got != current {
		t.Errorf("existing focus should be preserved; got %+v", got)
	}
}

func TestPickFocus_FallsBackOnMissing(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{{ID: "1", Unread: true}}
	if got := pickFocus(st, focusTarget{"prs", "gone#1"}); got != (focusTarget{"notifs", "1"}) {
		t.Errorf("missing focus should fall back to first unread; got %+v", got)
	}
}

func TestPickFocus_EmptyEverything(t *testing.T) {
	st := mkState(t)
	if got := pickFocus(st, focusTarget{}); got != (focusTarget{}) {
		t.Errorf("empty state should yield zero focus; got %+v", got)
	}
}

func TestPickFocus_AllReadFallsBackToFirstItem(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{{ID: "a", Unread: false}, {ID: "b", Unread: false}}
	if got := pickFocus(st, focusTarget{}); got != (focusTarget{"notifs", "a"}) {
		t.Errorf("all-read should fall back to first item; got %+v", got)
	}
}

func TestMoveFocus_AcrossPanels(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{{ID: "n1"}, {ID: "n2"}}
	st.LastPRs = []prs.PR{{Repo: "a/b", Number: 7}}
	// Status must be active for VisibleRows (used by cursorTargets) to keep
	// these runs; otherwise the cursor would skip the runs panel entirely.
	now := time.Now()
	st.LastView = []runs.Run{
		{ID: 9001, Status: "in_progress", UpdatedAt: now, CreatedAt: now},
		{ID: 9002, Status: "in_progress", UpdatedAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Second)},
	}

	// Order down through the flat list: n1 -> n2 -> a/b#7 -> 9001 -> 9002
	cases := []struct {
		from focusTarget
		want focusTarget
	}{
		{focusTarget{"notifs", "n1"}, focusTarget{"notifs", "n2"}},
		{focusTarget{"notifs", "n2"}, focusTarget{"prs", "a/b#7"}},
		{focusTarget{"prs", "a/b#7"}, focusTarget{"runs", "9001"}},
		{focusTarget{"runs", "9001"}, focusTarget{"runs", "9002"}},
		{focusTarget{"runs", "9002"}, focusTarget{"runs", "9002"}}, // clamped
	}
	for _, c := range cases {
		got := moveFocus(st, c.from, +1)
		if got != c.want {
			t.Errorf("moveFocus(%+v, +1) = %+v, want %+v", c.from, got, c.want)
		}
	}

	// Up from the first row stays put.
	if got := moveFocus(st, focusTarget{"notifs", "n1"}, -1); got != (focusTarget{"notifs", "n1"}) {
		t.Errorf("clamp at top: got %+v", got)
	}
	// Empty state returns zero value.
	if got := moveFocus(mkState(t), focusTarget{"notifs", "x"}, +1); got != (focusTarget{}) {
		t.Errorf("empty state should yield zero focus; got %+v", got)
	}
}

func TestFocusedURL_EachPanel(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{
		{ID: "n1", URL: "https://example.com/notif1"},
	}
	st.LastPRs = []prs.PR{
		{Repo: "a/b", Number: 7, URL: "https://example.com/pr7"},
	}
	st.LastView = []runs.Run{
		{ID: 9001, URL: "https://example.com/run9001"},
	}

	cases := []struct {
		name string
		f    focusTarget
		want string
	}{
		{"notif hit", focusTarget{"notifs", "n1"}, "https://example.com/notif1"},
		{"pr hit", focusTarget{"prs", "a/b#7"}, "https://example.com/pr7"},
		{"run hit", focusTarget{"runs", "9001"}, "https://example.com/run9001"},
		{"unknown panel", focusTarget{"other", "x"}, ""},
		{"missing notif", focusTarget{"notifs", "missing"}, ""},
		{"zero focus", focusTarget{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := focusedURL(st, c.f); got != c.want {
				t.Errorf("focusedURL = %q, want %q", got, c.want)
			}
		})
	}
}

// TestCursorTargets_HonoursVisibilityFilter is a regression test for the bug
// where arrow keys would land on workflow runs that VisibleRows filtered out
// (bots / >24h-completed / past the cap). cursorTargets must only emit rows
// that the UI is actually rendering, otherwise pressing ↓ feels "sticky".
func TestCursorTargets_HonoursVisibilityFilter(t *testing.T) {
	st := mkState(t)
	st.ViewerLogin = "me"
	now := time.Now()

	st.LastView = []runs.Run{
		// active any-actor: kept
		{ID: 1, Status: "in_progress", ActorLogin: "alice", UpdatedAt: now, CreatedAt: now},
		// completed by someone else: dropped
		{ID: 2, Status: "completed", Conclusion: "success", ActorLogin: "alice", UpdatedAt: now},
		// dependabot active: dropped
		{ID: 3, Status: "in_progress", ActorLogin: "dependabot[bot]", UpdatedAt: now},
		// recent my-success: kept
		{ID: 4, Status: "completed", Conclusion: "success", ActorLogin: "me", UpdatedAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour)},
	}

	got := cursorTargets(st)
	var runIDs []string
	for _, t := range got {
		if t.Panel == "runs" {
			runIDs = append(runIDs, t.ID)
		}
	}
	want := []string{"1", "4"}
	if !reflect.DeepEqual(runIDs, want) {
		t.Errorf("cursor target run IDs = %v, want %v (bot + completed-by-other should be filtered)", runIDs, want)
	}
}

func TestApplyRunDismiss_RemovesAndAdvancesToNext(t *testing.T) {
	st := mkState(t)
	st.ViewerLogin = "me"
	now := time.Now()
	// Three active runs visible in order 1, 2, 3 (UpdatedAt desc).
	st.LastView = []runs.Run{
		{ID: 1, Status: "in_progress", ActorLogin: "me", UpdatedAt: now, CreatedAt: now},
		{ID: 2, Status: "in_progress", ActorLogin: "me", UpdatedAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Second)},
		{ID: 3, Status: "in_progress", ActorLogin: "me", UpdatedAt: now.Add(-2 * time.Second), CreatedAt: now.Add(-2 * time.Second)},
	}
	// Dismiss the middle run; focus should advance to the same position (ID 3).
	got, ok := applyRunDismiss(st, "2")
	if !ok {
		t.Fatalf("expected ok=true for present ID")
	}
	if got != (focusTarget{"runs", "3"}) {
		t.Errorf("focus = %+v, want runs:3 (next row at same index)", got)
	}
	if len(st.LastView) != 2 {
		t.Errorf("expected 2 runs left in LastView, got %d", len(st.LastView))
	}
}

func TestApplyRunDismiss_ClampsAtEnd(t *testing.T) {
	st := mkState(t)
	st.ViewerLogin = "me"
	now := time.Now()
	st.LastView = []runs.Run{
		{ID: 1, Status: "in_progress", ActorLogin: "me", UpdatedAt: now, CreatedAt: now},
		{ID: 2, Status: "in_progress", ActorLogin: "me", UpdatedAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Second)},
	}
	// Dismiss the last visible run; focus should clamp to the new last.
	got, ok := applyRunDismiss(st, "2")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != (focusTarget{"runs", "1"}) {
		t.Errorf("focus = %+v, want runs:1 (clamped to new last)", got)
	}
}

func TestApplyRunDismiss_FallsBackWhenEmpty(t *testing.T) {
	st := mkState(t)
	st.ViewerLogin = "me"
	now := time.Now()
	st.LastView = []runs.Run{
		{ID: 1, Status: "in_progress", ActorLogin: "me", UpdatedAt: now, CreatedAt: now},
	}
	st.LastPRs = []prs.PR{{Repo: "a/b", Number: 7}}
	// Dismiss the only run; focus should fall back to the PRs panel.
	got, ok := applyRunDismiss(st, "1")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != (focusTarget{"prs", "a/b#7"}) {
		t.Errorf("focus = %+v, want prs:a/b#7 (fallback when runs empty)", got)
	}
}

func TestApplyRunDismiss_NotPresent(t *testing.T) {
	st := mkState(t)
	st.ViewerLogin = "me"
	now := time.Now()
	st.LastView = []runs.Run{
		{ID: 1, Status: "in_progress", ActorLogin: "me", UpdatedAt: now, CreatedAt: now},
	}
	got, ok := applyRunDismiss(st, "999")
	if ok {
		t.Errorf("expected ok=false for missing ID")
	}
	if got != (focusTarget{}) {
		t.Errorf("focus = %+v, want zero", got)
	}
	if len(st.LastView) != 1 {
		t.Errorf("state should be unchanged when ID not found; got %d runs", len(st.LastView))
	}
}

func TestApplyDismiss_RemovesAndAdvancesToNext(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{
		{ID: "1"}, {ID: "2"}, {ID: "3"},
	}
	got, ok := applyDismiss(st, "2")
	if !ok {
		t.Fatalf("expected ok=true for present ID")
	}
	if got != (focusTarget{"notifs", "3"}) {
		t.Errorf("focus = %+v, want notifs:3 (next row at same index)", got)
	}
	if len(st.LastNotifs) != 2 {
		t.Errorf("expected 2 notifs left, got %d", len(st.LastNotifs))
	}
}

func TestApplyDismiss_ClampsAtEnd(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{
		{ID: "1"}, {ID: "2"}, {ID: "3"},
	}
	// Dismiss the last item: focus should clamp to the new last item.
	got, ok := applyDismiss(st, "3")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != (focusTarget{"notifs", "2"}) {
		t.Errorf("focus = %+v, want notifs:2 (clamped to new last)", got)
	}
}

func TestApplyDismiss_FallsBackWhenEmpty(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{{ID: "1"}}
	st.LastPRs = []prs.PR{{Repo: "a/b", Number: 7}}
	// Dismiss the only notif: focus should fall through to the PR.
	got, ok := applyDismiss(st, "1")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != (focusTarget{"prs", "a/b#7"}) {
		t.Errorf("focus = %+v, want prs:a/b#7 (fallback)", got)
	}
}

func TestApplyDismiss_NotPresent(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{{ID: "1"}}
	got, ok := applyDismiss(st, "missing")
	if ok {
		t.Errorf("expected ok=false for missing ID")
	}
	if got != (focusTarget{}) {
		t.Errorf("focus = %+v, want zero", got)
	}
	if len(st.LastNotifs) != 1 {
		t.Errorf("state should be unchanged when not found; got %d notifs", len(st.LastNotifs))
	}
}
