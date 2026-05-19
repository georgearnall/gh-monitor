package ui

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/georgearnall/gha-monitor/internal/notifs"
	"github.com/georgearnall/gha-monitor/internal/prs"
	"github.com/georgearnall/gha-monitor/internal/runs"
)

func TestVisibleWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{"", 0},
		{"\x1b[31mhello\x1b[0m", 5},                                  // CSI red wrapping
		{"\x1b[1;31mbold red\x1b[0m", 8},                             // multi-param CSI
		{"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", 4},   // OSC 8 hyperlink with ST terminator
		{"\x1b]2;title\x07stuff", 5},                                 // OSC 2 with BEL terminator
		{"✓ pass", 6},                                                // multibyte but single rune ✓
		{"a✓b", 3},
	}
	for _, c := range cases {
		got := visibleWidth(c.in)
		if got != c.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestRelativeAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		offset time.Duration
		want   string
	}{
		{-30 * time.Second, "30s"},
		{-90 * time.Second, "1m"},
		{-5 * time.Hour, "5h"},
		{-49 * time.Hour, "2d"},
	}
	for _, c := range cases {
		got := relativeAge(now.Add(c.offset))
		if got != c.want {
			t.Errorf("relativeAge(now%+v) = %q, want %q", c.offset, got, c.want)
		}
	}
}

func TestAgeString(t *testing.T) {
	now := time.Now()
	// active run -> uses CreatedAt
	active := runs.Run{Status: "in_progress", CreatedAt: now.Add(-30 * time.Second), UpdatedAt: now.Add(-5 * time.Hour)}
	if got := ageString(active); got != "30s" {
		t.Errorf("active ageString = %q, want %q", got, "30s")
	}
	// terminal run -> uses UpdatedAt
	done := runs.Run{Status: "completed", CreatedAt: now.Add(-99 * time.Hour), UpdatedAt: now.Add(-3 * time.Minute)}
	if got := ageString(done); got != "3m" {
		t.Errorf("terminal ageString = %q, want %q", got, "3m")
	}
}

func TestTruncate(t *testing.T) {
	// truncate(s, n) returns up to n runes total: (n-1 source runes) + ellipsis.
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hell…"},
		{"短い", 5, "短い"},
		{"これは長い", 4, "これは…"},
	}
	for _, c := range cases {
		got := truncate(c.in, c.max)
		if got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestStatusCell_PlainWhenNotTTY(t *testing.T) {
	cases := []struct {
		run  runs.Run
		want string
	}{
		{runs.Run{Status: "in_progress"}, "● in_progress"},
		{runs.Run{Status: "completed", Conclusion: "success"}, "✓ success"},
		{runs.Run{Status: "completed", Conclusion: "failure"}, "✗ failure"},
		{runs.Run{Status: "completed", Conclusion: "cancelled"}, "· cancelled"},
	}
	for _, c := range cases {
		got := statusCell(c.run, false)
		if got != c.want {
			t.Errorf("statusCell(%+v, tty=false) = %q, want %q", c.run, got, c.want)
		}
	}
}

func TestPRStatusCell_PlainWhenNotTTY(t *testing.T) {
	cases := []struct {
		pr   prs.PR
		want string
	}{
		{prs.PR{State: "SUCCESS", Passing: 6, Total: 6}, "✓ 6/6 pass"},
		{prs.PR{State: "FAILURE", Failing: 1, Total: 2}, "✗ 1/2 fail"},
		{prs.PR{State: "PENDING", Passing: 2, Failing: 0, Total: 5}, "◐ 2/5 wait"},
		{prs.PR{State: "", Total: 0}, "· none"},
	}
	for _, c := range cases {
		got := prStatusCell(c.pr, false, false)
		if got != c.want {
			t.Errorf("prStatusCell(%+v, tty=false, compact=false) = %q, want %q", c.pr, got, c.want)
		}
	}
}

func TestPRStatusCell_CompactDropsText(t *testing.T) {
	cases := []struct {
		pr   prs.PR
		want string
	}{
		{prs.PR{State: "SUCCESS", Passing: 6, Total: 6}, "✓ 6/6"},
		{prs.PR{State: "FAILURE", Failing: 1, Total: 2}, "✗ 1/2"},
		{prs.PR{State: "PENDING", Passing: 2, Failing: 0, Total: 5}, "◐ 2/5"},
		{prs.PR{State: "", Total: 0}, "·"},
	}
	for _, c := range cases {
		got := prStatusCell(c.pr, false, true)
		if got != c.want {
			t.Errorf("prStatusCell(%+v, compact=true) = %q, want %q", c.pr, got, c.want)
		}
	}
}

func TestPRReviewCell_PlainWhenNotTTY(t *testing.T) {
	cases := []struct {
		name string
		pr   prs.PR
		want string
	}{
		{"approved", prs.PR{ReviewDecision: "APPROVED"}, "✓ approved"},
		{"changes requested", prs.PR{ReviewDecision: "CHANGES_REQUESTED"}, "✗ changes"},
		{"review required is blocked", prs.PR{ReviewDecision: "REVIEW_REQUIRED"}, "· blocked"},
		{"some reviews but no decision", prs.PR{ReviewCount: 3}, "◐ reviewed"},
		{"no reviews, no decision -> blank", prs.PR{}, ""},
		{"approved with comments", prs.PR{ReviewDecision: "APPROVED", CommentCount: 3}, "✓ approved +3"},
		{"no review but has comments shows just the comment count", prs.PR{CommentCount: 2}, "+2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := prReviewCell(c.pr, false)
			if got != c.want {
				t.Errorf("prReviewCell = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPrintAligned_PadsByVisibleWidth(t *testing.T) {
	// Status cell with ANSI escapes should not inflate the column width.
	rows := [][]string{
		{"STATUS", "REPO", "AGE"},
		{"\x1b[31m✗\x1b[0m 1/1 fail", "owner/repo", "2h"},
		{"\x1b[32m✓\x1b[0m pass", "x/y", "1d"},
	}
	out := captureStdout(t, func() { printAligned(rows) })
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), out)
	}
	// Find the rune position where REPO begins on each row. The visible position
	// should match across rows even though byte positions differ (the data rows
	// contain multi-byte ✗/✓ and invisible ANSI escapes).
	headerRepoCol := runeColOf(lines[0], "REPO")
	dataRepoNames := []string{"owner/repo", "x/y"}
	for i, name := range dataRepoNames {
		stripped := ansiRe.ReplaceAllString(lines[i+1], "")
		col := runeColOf(stripped, name)
		if col != headerRepoCol {
			t.Errorf("row %d: REPO at visible col %d, header at col %d (line=%q)", i+1, col, headerRepoCol, lines[i+1])
		}
	}
}

// runeColOf returns the rune-count offset at which substr first appears in s.
func runeColOf(s, substr string) int {
	idx := strings.Index(s, substr)
	if idx < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:idx])
}

func TestVisibleRows_FilterAndCap(t *testing.T) {
	now := time.Now()
	mk := func(id int64, status, conclusion, actor string, updatedOffset time.Duration) runs.Run {
		return runs.Run{
			ID:         id,
			Status:     status,
			Conclusion: conclusion,
			ActorLogin: actor,
			UpdatedAt:  now.Add(updatedOffset),
			CreatedAt:  now.Add(updatedOffset),
		}
	}

	viewer := "me"

	t.Run("active runs always kept regardless of actor", func(t *testing.T) {
		rs := []runs.Run{
			mk(1, "in_progress", "", "someone-else", -1*time.Minute),
			mk(2, "queued", "", "me", -1*time.Minute),
		}
		out := VisibleRows(rs, viewer)
		if len(out) != 2 {
			t.Errorf("got %d, want 2: %+v", len(out), out)
		}
	})

	t.Run("completed by other actor is dropped", func(t *testing.T) {
		rs := []runs.Run{
			mk(1, "completed", "failure", "someone-else", -5*time.Minute),
			mk(2, "completed", "success", "someone-else", -5*time.Minute),
		}
		out := VisibleRows(rs, viewer)
		if len(out) != 0 {
			t.Errorf("got %d, want 0: %+v", len(out), out)
		}
	})

	t.Run("completed by me kept for up to 24h", func(t *testing.T) {
		rs := []runs.Run{
			mk(1, "completed", "success", "me", -10*time.Minute),  // recent — keep
			mk(2, "completed", "failure", "me", -23*time.Hour),     // within 24h — keep
			mk(3, "completed", "success", "me", -25*time.Hour),     // outside 24h — drop
		}
		out := VisibleRows(rs, viewer)
		if len(out) != 2 {
			t.Errorf("got %d, want 2: %+v", len(out), out)
		}
		for _, r := range out {
			if r.ID == 3 {
				t.Errorf(">24h run leaked through")
			}
		}
	})

	t.Run("active sorted first, then by UpdatedAt desc", func(t *testing.T) {
		rs := []runs.Run{
			mk(1, "completed", "success", "me", -1*time.Minute),
			mk(2, "in_progress", "", "someone-else", -5*time.Hour),
			mk(3, "completed", "failure", "me", -30*time.Minute),
		}
		out := VisibleRows(rs, viewer)
		if len(out) != 3 {
			t.Fatalf("got %d, want 3", len(out))
		}
		if out[0].ID != 2 {
			t.Errorf("active should be first, got id=%d", out[0].ID)
		}
		if out[1].ID != 1 || out[2].ID != 3 {
			t.Errorf("completed should sort by UpdatedAt desc, got [%d, %d, %d]", out[0].ID, out[1].ID, out[2].ID)
		}
	})

	t.Run("caps at maxVisibleRuns", func(t *testing.T) {
		var rs []runs.Run
		// 15 active runs — all kept by filter, but truncated to 10.
		for i := 0; i < 15; i++ {
			rs = append(rs, mk(int64(i), "in_progress", "", "someone-else", time.Duration(-i)*time.Minute))
		}
		out := VisibleRows(rs, viewer)
		if len(out) != maxVisibleRuns {
			t.Errorf("got %d, want %d", len(out), maxVisibleRuns)
		}
	})

	t.Run("bot runs are filtered even when active or my-completed", func(t *testing.T) {
		rs := []runs.Run{
			// active but on a renovate branch -> drop
			{ID: 1, Status: "in_progress", Branch: "renovate/phpunit", ActorLogin: "someone-else", UpdatedAt: now},
			// completed by me, but copilot review -> drop
			{ID: 2, Status: "completed", Conclusion: "failure", WorkflowName: "Running Copilot Code Review", ActorLogin: "me", UpdatedAt: now.Add(-10 * time.Minute)},
			// dependabot actor -> drop
			{ID: 3, Status: "in_progress", ActorLogin: "dependabot[bot]", UpdatedAt: now},
			// genuine active run from someone else -> keep
			{ID: 4, Status: "in_progress", ActorLogin: "alice", Branch: "feature/x", WorkflowName: "CI", UpdatedAt: now},
		}
		out := VisibleRows(rs, viewer)
		if len(out) != 1 || out[0].ID != 4 {
			t.Errorf("got %+v, want only id=4", out)
		}
	})

	t.Run("no viewer login means only active runs survive", func(t *testing.T) {
		rs := []runs.Run{
			mk(1, "in_progress", "", "me", -1*time.Minute),
			mk(2, "completed", "success", "me", -5*time.Minute),
		}
		out := VisibleRows(rs, "")
		if len(out) != 1 || out[0].ID != 1 {
			t.Errorf("got %+v, want only id=1", out)
		}
	})
}

func TestReasonCell_PlainWhenNotTTY_Unread(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"mention", "@ mention"},
		{"team_mention", "@ mention"},
		{"review_requested", "◐ review"},
		{"comment", "+ comment"},
		{"author", "· own"},
		{"assign", "· own"},
		{"subscribed", "· subscribed"},
	}
	for _, c := range cases {
		got := reasonCell(notifs.Notification{Reason: c.reason, Unread: true}, false)
		if got != c.want {
			t.Errorf("reasonCell(%q, unread=true, tty=false) = %q, want %q", c.reason, got, c.want)
		}
	}
}

func TestReasonCell_ReadVariant(t *testing.T) {
	// Read notifications return plain (no colour) so the caller can dim the
	// whole row consistently.
	got := reasonCell(notifs.Notification{Reason: "mention", Unread: false}, true)
	if got != "@ mention" {
		t.Errorf("read mention reasonCell = %q, want %q", got, "@ mention")
	}
}

func TestUnreadCount(t *testing.T) {
	ns := []notifs.Notification{
		{Unread: true}, {Unread: false}, {Unread: true}, {Unread: true},
	}
	if got := unreadCount(ns); got != 3 {
		t.Errorf("unreadCount = %d, want 3", got)
	}
	if got := unreadCount(nil); got != 0 {
		t.Errorf("unreadCount(nil) = %d, want 0", got)
	}
}

func TestVisibleNotifs_CapsAtMax(t *testing.T) {
	var ns []notifs.Notification
	for i := 0; i < maxVisibleNotifs+5; i++ {
		ns = append(ns, notifs.Notification{ID: "x"})
	}
	if got := VisibleNotifs(ns); len(got) != maxVisibleNotifs {
		t.Errorf("got %d, want %d", len(got), maxVisibleNotifs)
	}
}

func TestWindowTitleString(t *testing.T) {
	cases := []struct {
		unread, active, failed int
		want                   string
	}{
		{0, 0, 0, "gha-monitor · 0 active · 0 recent failures"},
		{0, 2, 1, "gha-monitor · 2 active · 1 recent failures"},
		{3, 2, 1, "gha-monitor · 3 unread · 2 active · 1 recent failures"},
	}
	for _, c := range cases {
		got := windowTitleString(c.unread, c.active, c.failed)
		if got != c.want {
			t.Errorf("windowTitleString(%d,%d,%d) = %q, want %q", c.unread, c.active, c.failed, got, c.want)
		}
	}
}

func TestWriteNotifsTable_AlignsWithDimmedRows(t *testing.T) {
	now := time.Now()
	ns := []notifs.Notification{
		{ID: "1", Repo: "acme/billing", PRNumber: 88, Title: "Add VAT", Reason: "review_requested", URL: "https://github.com/acme/billing/pull/88", UpdatedAt: now.Add(-5 * time.Minute), Unread: true},
		{ID: "2", Repo: "acme/legacy", PRNumber: 35, Title: "Tidy", Reason: "mention", URL: "https://github.com/acme/legacy/pull/35", UpdatedAt: now.Add(-2 * time.Hour), Unread: false},
	}
	out := captureStdout(t, func() { writeNotifsTable(ns, "", true, 0) })
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), out)
	}
	// Both header REPO and both data REPO cells should align at the same visible column.
	headerCol := runeColOf(ansiRe.ReplaceAllString(lines[0], ""), "REPO")
	for i, repo := range []string{"acme/billing", "acme/legacy"} {
		col := runeColOf(ansiRe.ReplaceAllString(lines[i+1], ""), repo)
		if col != headerCol {
			t.Errorf("row %d: %q at col %d, header REPO at col %d", i+1, repo, col, headerCol)
		}
	}
}

func TestShrinkRepo(t *testing.T) {
	cases := []struct {
		name   string
		repo   string
		budget int
		want   string
	}{
		{"fits as-is", "acme/billing", 20, "acme/billing"},
		{"already short", "a/b", 5, "a/b"},
		// Stage 1 of the cascade: keep some owner via "<stub>…/<name>".
		// name="partnerships-api" (16 chars), so stub = budget - 18.
		{"owner trimmed with 4-char stub", "trainline-private/partnerships-api", 22, "trai…/partnerships-api"},
		{"owner stub of 1 char", "trainline-private/partnerships-api", 19, "t…/partnerships-api"},
		// Stage 2: drop the org entirely when there's no useful stub room.
		{"drops org when stub would be <1", "trainline-private/foo", 4, "foo"},
		{"drops org and shows just name", "trainline-private/partnerships-api", 17, "partnerships-api"},
		// Stage 3: name itself doesn't fit -> generic truncate of the name.
		{"truncates name when name is too long", "trainline-private/partnerships-api", 12, "partnership…"},
		// No slash: fall through to plain truncate.
		{"no slash", "unparseable-thing", 10, "unparseab…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shrinkRepo(c.repo, c.budget)
			if got != c.want {
				t.Errorf("shrinkRepo(%q, %d) = %q, want %q", c.repo, c.budget, got, c.want)
			}
		})
	}
}

func TestFitRepoColumn_NoopWhenFits(t *testing.T) {
	rows := [][]string{
		{"  ", "REASON", "TITLE", "REPO", "#"},
		{"  ", "@ m", "Short title", "acme/billing", "#1"},
	}
	fitRepoColumn(rows, 3, 200)
	if rows[1][3] != "acme/billing" {
		t.Errorf("repo cell should be untouched when termWidth is generous: got %q", rows[1][3])
	}
}

func TestFitRepoColumn_ShrinksWhenTight(t *testing.T) {
	rows := [][]string{
		{"  ", "REASON", "TITLE", "REPO", "#"},
		{"  ", "@ mention", "Lorem ipsum dolor sit amet", "trainline-private/partnerships-api", "#42"},
	}
	// Natural width: 2 + 9 + 26 + 34 + 3 = 74 + 4*2 = 82. With termWidth=70
	// the repo column has 22 chars of budget — enough to preserve the full
	// name suffix while trimming the owner.
	fitRepoColumn(rows, 3, 70)
	if rows[1][3] == "trainline-private/partnerships-api" {
		t.Errorf("repo cell should have been shrunk; got %q", rows[1][3])
	}
	if !strings.HasSuffix(rows[1][3], "/partnerships-api") {
		t.Errorf("repo cell should preserve the full name suffix; got %q", rows[1][3])
	}
}

func TestFitRepoColumn_FallsBackToPlainTruncateWhenTooTight(t *testing.T) {
	rows := [][]string{
		{"  ", "TITLE", "REPO"},
		{"  ", "Lorem ipsum dolor sit amet", "trainline-private/very-long-repository-name"},
	}
	// Tight budget can't fit "/very-long-repository-name" suffix.
	fitRepoColumn(rows, 2, 35)
	if rows[1][2] == "trainline-private/very-long-repository-name" {
		t.Errorf("repo cell should have been shrunk; got %q", rows[1][2])
	}
	if !strings.HasSuffix(rows[1][2], "…") {
		t.Errorf("expected plain ellipsis truncation when too tight; got %q", rows[1][2])
	}
}

func TestFitRepoColumn_NoopOnZeroWidth(t *testing.T) {
	rows := [][]string{
		{"a", "b", "c", "trainline-private/foo"},
		{"a", "b", "c", "trainline-private/foo"},
	}
	fitRepoColumn(rows, 3, 0)
	if rows[1][3] != "trainline-private/foo" {
		t.Errorf("termWidth=0 means unconstrained; got %q", rows[1][3])
	}
}

func TestColumnOrdering(t *testing.T) {
	// Verify TITLE/WORKFLOW appears before REPO in the header for each table.
	now := time.Now()

	notifsOut := captureStdout(t, func() {
		writeNotifsTable([]notifs.Notification{
			{ID: "1", Repo: "a/b", PRNumber: 1, Title: "test", Reason: "mention", URL: "u", UpdatedAt: now, Unread: true},
		}, "", true, 0)
	})
	if !columnOrderOK(notifsOut, []string{"TITLE", "REPO"}) {
		t.Errorf("notifs: TITLE must come before REPO\n%s", notifsOut)
	}

	prOut := captureStdout(t, func() {
		writePRTable([]prs.PR{
			{Repo: "a/b", Number: 1, Title: "test", URL: "u", UpdatedAt: now},
		}, "", true, 0)
	})
	if !columnOrderOK(prOut, []string{"TITLE", "REPO"}) {
		t.Errorf("prs: TITLE must come before REPO\n%s", prOut)
	}

	runsOut := captureStdout(t, func() {
		writeTable([]runs.Run{
			{ID: 1, Repo: "a/b", WorkflowName: "CI", Status: "in_progress", URL: "u", CreatedAt: now, UpdatedAt: now},
		}, "", true, 0)
	})
	if !columnOrderOK(runsOut, []string{"WORKFLOW", "REPO"}) {
		t.Errorf("runs: WORKFLOW must come before REPO\n%s", runsOut)
	}
}

func columnOrderOK(out string, order []string) bool {
	stripped := ansiRe.ReplaceAllString(out, "")
	lastIdx := -1
	for _, label := range order {
		idx := strings.Index(stripped, label)
		if idx < 0 || idx <= lastIdx {
			return false
		}
		lastIdx = idx
	}
	return true
}

func TestWriteNotifsTable_FocusCursor(t *testing.T) {
	now := time.Now()
	ns := []notifs.Notification{
		{ID: "1", Repo: "acme/a", PRNumber: 1, Title: "one", Reason: "mention", URL: "https://github.com/acme/a/pull/1", UpdatedAt: now, Unread: true},
		{ID: "2", Repo: "acme/b", PRNumber: 2, Title: "two", Reason: "mention", URL: "https://github.com/acme/b/pull/2", UpdatedAt: now, Unread: true},
	}
	out := captureStdout(t, func() { writeNotifsTable(ns, "2", true, 0) })
	stripped := ansiRe.ReplaceAllString(out, "")
	if !strings.Contains(stripped, "▶") {
		t.Fatalf("expected cursor glyph in output:\n%s", stripped)
	}
	// Cursor must be on the focused row, not the other one.
	for _, line := range strings.Split(strings.TrimRight(stripped, "\n"), "\n") {
		if strings.Contains(line, "▶") && !strings.Contains(line, "acme/b") {
			t.Errorf("cursor on wrong row: %q", line)
		}
	}

	// And no cursor when focusedID is empty.
	out = captureStdout(t, func() { writeNotifsTable(ns, "", true, 0) })
	if strings.Contains(ansiRe.ReplaceAllString(out, ""), "▶") {
		t.Errorf("expected no cursor when focusedID empty:\n%s", out)
	}
}

func TestWritePRTable_AdaptiveDropsBranchOnOverflow(t *testing.T) {
	now := time.Now()
	ps := []prs.PR{
		{
			Repo: "trainline-private/VatCalculationService", Number: 88,
			Title:      "Ensure tests cover corporate onboarding for VAT calculation",
			HeadBranch: "test-corporateonboarding",
			URL:        "u",
			UpdatedAt:  now,
			State:      "FAILURE", Failing: 1, Total: 1,
		},
	}

	// At a generous width the natural table fits and BRANCH stays.
	wide := captureStdout(t, func() { writePRTable(ps, "", true, 200) })
	if !strings.Contains(wide, "BRANCH") {
		t.Errorf("BRANCH header should remain at width=200:\n%s", wide)
	}
	if !strings.Contains(wide, "test-corporateonboarding") {
		t.Errorf("BRANCH cell should remain at width=200:\n%s", wide)
	}

	// At a width that would force aggressive repo truncation, BRANCH is
	// the first column to go.
	narrow := captureStdout(t, func() { writePRTable(ps, "", true, 110) })
	if strings.Contains(narrow, "BRANCH") {
		t.Errorf("BRANCH header should drop when too narrow:\n%s", narrow)
	}
	if strings.Contains(narrow, "test-corporateonboarding") {
		t.Errorf("BRANCH cell should drop when too narrow:\n%s", narrow)
	}
}

func TestWritePRTable_VeryNarrowAlsoCompactsStatus(t *testing.T) {
	now := time.Now()
	ps := []prs.PR{
		{
			Repo: "x/y", Number: 1, Title: "t", URL: "u", UpdatedAt: now,
			State: "FAILURE", Failing: 1, Total: 1,
		},
	}
	// Below the natural width even without BRANCH, status should compact.
	out := captureStdout(t, func() { writePRTable(ps, "", true, 40) })
	stripped := ansiRe.ReplaceAllString(out, "")
	if strings.Contains(stripped, "1/1 fail") {
		t.Errorf("status word 'fail' should drop at width=40:\n%s", stripped)
	}
	if !strings.Contains(stripped, "1/1") {
		t.Errorf("status ratio should still be present:\n%s", stripped)
	}
}

func TestStyleRepoCell(t *testing.T) {
	cases := []struct {
		name string
		in   string
		tty  bool
		want string
	}{
		{"non-tty is no-op", "trainline/foo", false, "trainline/foo"},
		{"owner+slash wrapped in dim", "trainline/foo", true, ansiDim + "trainline/" + ansiReset + "foo"},
		{"already-dropped org untouched", "foo", true, "foo"},
		{"truncated owner with ellipsis still gets dimmed", "tra…/foo", true, ansiDim + "tra…/" + ansiReset + "foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := styleRepoCell(c.in, c.tty)
			if got != c.want {
				t.Errorf("styleRepoCell(%q, tty=%v) = %q, want %q", c.in, c.tty, got, c.want)
			}
		})
	}
}

func TestStyleRepoCell_VisibleWidthUnchanged(t *testing.T) {
	// Adding ANSI codes must not affect alignment math.
	raw := "trainline-private/foo"
	styled := styleRepoCell(raw, true)
	if visibleWidth(raw) != visibleWidth(styled) {
		t.Errorf("visible width mismatch: raw=%d styled=%d", visibleWidth(raw), visibleWidth(styled))
	}
}

func TestWritePRTable_DimsOrganisation(t *testing.T) {
	now := time.Now()
	ps := []prs.PR{
		{Repo: "trainline-private/foo", Number: 1, Title: "test", URL: "u", UpdatedAt: now},
	}
	out := captureStdout(t, func() { writePRTable(ps, "", true, 300) })
	if !strings.Contains(out, ansiDim+"trainline-private/"+ansiReset+"foo") {
		t.Errorf("expected owner+slash dimmed; output:\n%q", out)
	}
}

func TestWriteNotifsTable_UnreadGetsDimOrg_ReadIsRowDim(t *testing.T) {
	now := time.Now()
	ns := []notifs.Notification{
		{ID: "1", Repo: "trainline-private/foo", PRNumber: 1, Title: "t", Reason: "mention", URL: "u", UpdatedAt: now, Unread: true},
		{ID: "2", Repo: "trainline-private/bar", PRNumber: 2, Title: "t", Reason: "mention", URL: "u", UpdatedAt: now, Unread: false},
	}
	out := captureStdout(t, func() { writeNotifsTable(ns, "", true, 300) })
	// Unread row: owner+slash wrapped in dim, rest bright.
	if !strings.Contains(out, ansiDim+"trainline-private/"+ansiReset+"foo") {
		t.Errorf("unread row should have owner dimmed; output:\n%q", out)
	}
	// Read row: whole row wrapped, so the dimmed-owner pattern should NOT
	// appear (it would put a reset in the middle and break the row dim).
	if strings.Contains(out, ansiDim+"trainline-private/"+ansiReset+"bar") {
		t.Errorf("read row should not have inner owner-dim (would break row dim); output:\n%q", out)
	}
}

func TestWriteTable_DimsOrganisation(t *testing.T) {
	now := time.Now()
	rs := []runs.Run{
		{ID: 1, Repo: "trainline-private/foo", WorkflowName: "CI", Status: "in_progress", URL: "u", CreatedAt: now, UpdatedAt: now},
	}
	out := captureStdout(t, func() { writeTable(rs, "", true, 300) })
	if !strings.Contains(out, ansiDim+"trainline-private/"+ansiReset+"foo") {
		t.Errorf("expected owner+slash dimmed in workflow runs table; output:\n%q", out)
	}
}

func TestWritePRTable_TitleTruncatedAt55(t *testing.T) {
	now := time.Now()
	title := strings.Repeat("a", 80) // longer than the 55-char cap
	ps := []prs.PR{
		{Repo: "x/y", Number: 1, Title: title, URL: "u", UpdatedAt: now},
	}
	out := captureStdout(t, func() { writePRTable(ps, "", true, 300) })
	stripped := ansiRe.ReplaceAllString(out, "")
	// 54 'a's + ellipsis = 55 visible runes.
	wantTitle := strings.Repeat("a", 54) + "…"
	if !strings.Contains(stripped, wantTitle) {
		t.Errorf("expected title truncated to 55 runes; got:\n%s", stripped)
	}
}

func TestWritePRTable_FocusCursor(t *testing.T) {
	now := time.Now()
	ps := []prs.PR{
		{Repo: "acme/a", Number: 1, Title: "one", URL: "https://github.com/acme/a/pull/1", UpdatedAt: now},
		{Repo: "acme/b", Number: 2, Title: "two", URL: "https://github.com/acme/b/pull/2", UpdatedAt: now},
	}
	out := captureStdout(t, func() { writePRTable(ps, "acme/b#2", true, 0) })
	stripped := ansiRe.ReplaceAllString(out, "")
	if !strings.Contains(stripped, "▶") {
		t.Fatalf("expected cursor glyph in output:\n%s", stripped)
	}
	for _, line := range strings.Split(strings.TrimRight(stripped, "\n"), "\n") {
		if strings.Contains(line, "▶") && !strings.Contains(line, "acme/b") {
			t.Errorf("PR cursor on wrong row: %q", line)
		}
	}

	out = captureStdout(t, func() { writePRTable(ps, "", true, 0) })
	if strings.Contains(ansiRe.ReplaceAllString(out, ""), "▶") {
		t.Errorf("expected no PR cursor when focusedKey empty:\n%s", out)
	}
}

func TestWriteTable_FocusCursor(t *testing.T) {
	now := time.Now()
	rs := []runs.Run{
		{ID: 9001, Repo: "acme/a", WorkflowName: "CI", Status: "in_progress", URL: "https://github.com/acme/a/runs/9001", CreatedAt: now, UpdatedAt: now},
		{ID: 9002, Repo: "acme/b", WorkflowName: "Deploy", Status: "in_progress", URL: "https://github.com/acme/b/runs/9002", CreatedAt: now, UpdatedAt: now},
	}
	out := captureStdout(t, func() { writeTable(rs, "9002", true, 0) })
	stripped := ansiRe.ReplaceAllString(out, "")
	if !strings.Contains(stripped, "▶") {
		t.Fatalf("expected cursor glyph in output:\n%s", stripped)
	}
	for _, line := range strings.Split(strings.TrimRight(stripped, "\n"), "\n") {
		if strings.Contains(line, "▶") && !strings.Contains(line, "acme/b") {
			t.Errorf("run cursor on wrong row: %q", line)
		}
	}

	out = captureStdout(t, func() { writeTable(rs, "", true, 0) })
	if strings.Contains(ansiRe.ReplaceAllString(out, ""), "▶") {
		t.Errorf("expected no run cursor when focusedID empty:\n%s", out)
	}
}

// captureStdout temporarily redirects os.Stdout to a pipe and returns whatever
// fn wrote to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()
	w.Close()
	os.Stdout = old
	return <-done
}
