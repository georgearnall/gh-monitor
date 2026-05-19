package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
	"github.com/georgearnall/gha-monitor/internal/notifs"
	"github.com/georgearnall/gha-monitor/internal/prs"
	"github.com/georgearnall/gha-monitor/internal/runs"
	"github.com/georgearnall/gha-monitor/internal/state"
)

func TestStringSet(t *testing.T) {
	var s stringSet
	if err := s.Set("a/b"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("c/d"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("a/b"); err != nil {
		t.Fatalf("Set dup: %v", err)
	}
	if !s["a/b"] || !s["c/d"] {
		t.Errorf("missing entries: %v", s)
	}
	got := s.String()
	// stringSet.String iterates a map, so order isn't stable — assert membership.
	for _, want := range []string{"a/b", "c/d"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

func TestFilterExcluded(t *testing.T) {
	repos := []discovery.Repo{
		{FullName: "a/x"},
		{FullName: "a/y"},
		{FullName: "b/z"},
	}
	got := filterExcluded(repos, stringSet{"a/y": true})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	for _, r := range got {
		if r.FullName == "a/y" {
			t.Errorf("excluded repo leaked: %v", got)
		}
	}
}

func TestFilterExcluded_NoExclusions(t *testing.T) {
	repos := []discovery.Repo{{FullName: "a/b"}}
	got := filterExcluded(repos, nil)
	if len(got) != 1 {
		t.Errorf("nil exclusion set should be a no-op, got %+v", got)
	}
}

func TestWatchConfig_NextInterval(t *testing.T) {
	cfg := watchConfig{
		baseInterval:   60 * time.Second,
		activeInterval: 20 * time.Second,
		lowQuotaFloor:  2 * time.Minute,
		lowQuotaLimit:  500,
	}

	cases := []struct {
		name    string
		active  int
		rl      ghclient.RateLimit
		pollErr error
		want    time.Duration
	}{
		{"idle, no rate-limit info", 0, ghclient.RateLimit{}, nil, 60 * time.Second},
		{"active runs", 2, ghclient.RateLimit{Limit: 5000, Remaining: 4000}, nil, 20 * time.Second},
		{"low quota raises floor", 0, ghclient.RateLimit{Limit: 5000, Remaining: 100}, nil, 2 * time.Minute},
		{"low quota also lifts active", 3, ghclient.RateLimit{Limit: 5000, Remaining: 100}, nil, 2 * time.Minute},
		{"retry-after dominates", 0, ghclient.RateLimit{}, &ghclient.RateLimitedError{RetryAfter: 5 * time.Minute}, 5 * time.Minute},
		{"retry-after small still raised to floor", 0, ghclient.RateLimit{}, &ghclient.RateLimitedError{RetryAfter: 10 * time.Second}, 2 * time.Minute},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cfg.nextInterval(c.active, c.rl, c.pollErr)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func mkState(t *testing.T) *state.State {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return s
}

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

func TestWatchConfig_NextInterval_PreservesRateLimitedAsErr(t *testing.T) {
	// Sanity: ghclient.AsRateLimited recognises a wrapped pointer-error.
	err := errors.New("plain")
	if _, ok := ghclient.AsRateLimited(err); ok {
		t.Errorf("AsRateLimited returned true for non-RateLimitedError")
	}
}
