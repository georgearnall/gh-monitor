package main

import (
	"os"
	"path/filepath"
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

func TestFocusedRepo(t *testing.T) {
	st := mkState(t)
	st.LastNotifs = []notifs.Notification{{ID: "1", Repo: "acme/notifs-repo"}}
	st.LastPRs = []prs.PR{{Repo: "acme/prs-repo", Number: 7}}
	st.LastAssignedPRs = []prs.PR{{Repo: "acme/assigned-repo", Number: 9}}
	st.LastView = []runs.Run{{ID: 42, Repo: "acme/runs-repo"}}

	cases := []struct {
		name string
		f    focusTarget
		want string
	}{
		{"notif", focusTarget{"notifs", "1"}, "acme/notifs-repo"},
		{"pr", focusTarget{"prs", "acme/prs-repo#7"}, "acme/prs-repo"},
		{"assigned pr", focusTarget{"prs", "acme/assigned-repo#9"}, "acme/assigned-repo"},
		{"run", focusTarget{"runs", "42"}, "acme/runs-repo"},
		{"unknown", focusTarget{"notifs", "missing"}, ""},
		{"zero", focusTarget{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := focusedRepo(st, c.f); got != c.want {
				t.Errorf("focusedRepo(%+v) = %q, want %q", c.f, got, c.want)
			}
		})
	}
}

func TestResolveRepoPath(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, dir+"/flat-repo/.git")
	mustMkdirAll(t, dir+"/acme/nested-repo/.git")
	mustMkdirAll(t, dir+"/not-a-repo") // no .git

	cases := []struct {
		name     string
		fullName string
		wantOK   bool
		wantRel  string // path relative to dir, only checked when wantOK
	}{
		{"flat layout", "acme/flat-repo", true, "flat-repo"},
		{"nested layout", "acme/nested-repo", true, "acme/nested-repo"},
		{"no .git", "acme/not-a-repo", false, ""},
		{"not cloned", "acme/missing-repo", false, ""},
		{"empty full name", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := resolveRepoPath(dir, c.fullName)
			if ok != c.wantOK {
				t.Fatalf("resolveRepoPath(%q, %q) ok = %v, want %v (path %q)", dir, c.fullName, ok, c.wantOK, got)
			}
			if ok && got != dir+"/"+c.wantRel {
				t.Errorf("resolveRepoPath(%q, %q) = %q, want %q", dir, c.fullName, got, dir+"/"+c.wantRel)
			}
		})
	}

	if _, ok := resolveRepoPath("", "acme/flat-repo"); ok {
		t.Errorf("empty sourceDir should never resolve")
	}
}

// TestExpandHome is a regression test: the repo source directory is typed
// into the app's own inline prompt (not a shell), so a leading "~" never
// gets expanded for us and must be resolved by hand.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/source", filepath.Join(home, "source")},
		{"~/source/nested", filepath.Join(home, "source", "nested")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"", ""},
	}
	for _, c := range cases {
		if got := expandHome(c.in); got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveRepoPath_TildeExpansion is a regression test for the reported
// bug: typing "~/source" into the prompt resolved nothing because the
// literal "~" was joined straight into the filesystem path.
func TestResolveRepoPath_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	dir, err := os.MkdirTemp(home, "gh-monitor-tilde-test-*")
	if err != nil {
		t.Fatalf("mkdirtemp under home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	mustMkdirAll(t, filepath.Join(dir, "flat-repo", ".git"))

	rel, err := filepath.Rel(home, dir)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	tildeDir := "~/" + rel

	got, ok := resolveRepoPath(tildeDir, "acme/flat-repo")
	if !ok {
		t.Fatalf("resolveRepoPath(%q, ...) ok = false, want true", tildeDir)
	}
	want := filepath.Join(dir, "flat-repo")
	if got != want {
		t.Errorf("resolveRepoPath(%q, ...) = %q, want %q", tildeDir, got, want)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}
