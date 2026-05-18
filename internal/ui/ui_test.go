package ui

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		got := prStatusCell(c.pr, false)
		if got != c.want {
			t.Errorf("prStatusCell(%+v, false) = %q, want %q", c.pr, got, c.want)
		}
	}
}

func TestPRReviewCell_PlainWhenNotTTY(t *testing.T) {
	cases := []struct {
		pr   prs.PR
		want string
	}{
		{prs.PR{ReviewDecision: "APPROVED"}, "✓ approved"},
		{prs.PR{ReviewDecision: "CHANGES_REQUESTED"}, "✗ changes"},
		{prs.PR{ReviewDecision: "REVIEW_REQUIRED"}, "· needs review"},
		{prs.PR{ReviewCount: 3}, "◐ reviewed"},
		{prs.PR{}, "· no review"},
		{prs.PR{ReviewDecision: "APPROVED", CommentCount: 3}, "✓ approved +3"},
	}
	for _, c := range cases {
		got := prReviewCell(c.pr, false)
		if got != c.want {
			t.Errorf("prReviewCell(%+v, false) = %q, want %q", c.pr, got, c.want)
		}
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
		out := visibleRows(rs, viewer)
		if len(out) != 2 {
			t.Errorf("got %d, want 2: %+v", len(out), out)
		}
	})

	t.Run("completed by other actor is dropped", func(t *testing.T) {
		rs := []runs.Run{
			mk(1, "completed", "failure", "someone-else", -5*time.Minute),
			mk(2, "completed", "success", "someone-else", -5*time.Minute),
		}
		out := visibleRows(rs, viewer)
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
		out := visibleRows(rs, viewer)
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
		out := visibleRows(rs, viewer)
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
		out := visibleRows(rs, viewer)
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
		out := visibleRows(rs, viewer)
		if len(out) != 1 || out[0].ID != 4 {
			t.Errorf("got %+v, want only id=4", out)
		}
	})

	t.Run("no viewer login means only active runs survive", func(t *testing.T) {
		rs := []runs.Run{
			mk(1, "in_progress", "", "me", -1*time.Minute),
			mk(2, "completed", "success", "me", -5*time.Minute),
		}
		out := visibleRows(rs, "")
		if len(out) != 1 || out[0].ID != 1 {
			t.Errorf("got %+v, want only id=1", out)
		}
	})
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
