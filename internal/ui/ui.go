package ui

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/georgearnall/gha-monitor/internal/notifs"
	"github.com/georgearnall/gha-monitor/internal/prs"
	"github.com/georgearnall/gha-monitor/internal/runs"
)

const (
	ansiClear  = "\x1b[H\x1b[2J"
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiGreen  = "\x1b[32m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

type Snapshot struct {
	Runs          []runs.Run
	PRs           []prs.PR
	Notifs        []notifs.Notification
	ViewerLogin   string // authenticated user's login, used to keep their own runs longer
	RepoCount     int
	RateRemaining int
	RateLimit     int
	PolledAt      time.Time
	NextPollIn    time.Duration
	Stale         bool // rendering from disk cache, not fresh
	Refreshing    bool // a background refresh is in flight
}

// Render redraws the status table. Safe to call when stdout is not a tty;
// ANSI control codes are suppressed in that case.
func Render(snap Snapshot) {
	tty := isTTY(os.Stdout)

	rows := visibleRows(snap.Runs, snap.ViewerLogin)
	active, failed := countByOutcome(rows)
	notifRows := visibleNotifs(snap.Notifs)
	unread := unreadCount(notifRows)

	if tty {
		fmt.Print(ansiClear)
		setWindowTitle(windowTitleString(unread, active, failed))
	}

	header(snap, tty)

	if len(notifRows) > 0 {
		fmt.Println()
		fmt.Println(dim("NOTIFICATIONS", tty))
		writeNotifsTable(notifRows, tty)
	}

	if len(snap.PRs) > 0 {
		fmt.Println()
		fmt.Println(dim("PULL REQUESTS", tty))
		writePRTable(snap.PRs, tty)
	}

	if len(rows) > 0 || (len(snap.PRs) == 0 && len(notifRows) == 0) {
		fmt.Println()
		if len(snap.PRs) > 0 || len(notifRows) > 0 {
			fmt.Println(dim("WORKFLOW RUNS", tty))
		}
		if len(rows) == 0 {
			fmt.Println(dim("no active runs or recent failures", tty))
		} else {
			writeTable(rows, tty)
		}
	}
	footer(snap, tty)
}

func windowTitleString(unread, active, failed int) string {
	if unread > 0 {
		return fmt.Sprintf("gha-monitor · %d unread · %d active · %d recent failures", unread, active, failed)
	}
	return fmt.Sprintf("gha-monitor · %d active · %d recent failures", active, failed)
}

func header(snap Snapshot, tty bool) {
	title := "gha-monitor"
	if tty {
		title = ansiBold + title + ansiReset
	}
	if snap.Refreshing {
		title += dim(" · refreshing", tty)
	}
	fmt.Println(title)
}

func footer(snap Snapshot, tty bool) {
	parts := []string{polledLabel(snap)}
	parts = append(parts, fmt.Sprintf("%d repos", snap.RepoCount))
	if len(snap.Notifs) > 0 {
		parts = append(parts, fmt.Sprintf("%d notifs", len(snap.Notifs)))
	}
	if len(snap.PRs) > 0 {
		parts = append(parts, fmt.Sprintf("%d PRs", len(snap.PRs)))
	}
	if snap.RateLimit > 0 {
		parts = append(parts, fmt.Sprintf("rate limit %d/%d", snap.RateRemaining, snap.RateLimit))
	}
	if snap.Refreshing {
		parts = append(parts, "refreshing…")
	} else if snap.NextPollIn > 0 {
		parts = append(parts, fmt.Sprintf("next poll in %s", snap.NextPollIn.Round(time.Second)))
	}
	fmt.Println()
	fmt.Println(dim(join(parts, " · "), tty))
	if tty {
		fmt.Println(dim("[r] refresh  [q] quit", tty))
	}
}

func polledLabel(snap Snapshot) string {
	if snap.PolledAt.IsZero() {
		return "polled never"
	}
	if snap.Stale {
		return "polled " + relativeAge(snap.PolledAt) + " ago"
	}
	return "polled " + snap.PolledAt.Format("15:04:05")
}

func relativeAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// maxVisibleNotifs caps the notifications section length.
const maxVisibleNotifs = 15

// visibleNotifs slices the notifications list to maxVisibleNotifs. Filtering
// and sorting already happened upstream in notifs.Poll.
func visibleNotifs(ns []notifs.Notification) []notifs.Notification {
	if len(ns) > maxVisibleNotifs {
		return ns[:maxVisibleNotifs]
	}
	return ns
}

func unreadCount(ns []notifs.Notification) int {
	n := 0
	for _, x := range ns {
		if x.Unread {
			n++
		}
	}
	return n
}

func writeNotifsTable(ns []notifs.Notification, tty bool) {
	rows := make([][]string, 0, len(ns)+1)
	rows = append(rows, dimRow([]string{"REASON", "REPO", "#", "TITLE", "AGE", "LINK"}, tty))
	for _, n := range ns {
		link := n.URL
		if tty {
			link = hyperlink(n.URL, "open ↗")
		}
		cells := []string{
			reasonCell(n, tty),
			n.Repo,
			fmt.Sprintf("#%d", n.PRNumber),
			truncate(n.Title, 50),
			relativeAge(n.UpdatedAt),
			link,
		}
		if !n.Unread && tty {
			for i, c := range cells {
				cells[i] = ansiDim + c + ansiReset
			}
		}
		rows = append(rows, cells)
	}
	printAligned(rows)
}

// reasonCell formats the notification reason. Unread items keep their colour;
// read items return plain text so the caller can wrap the whole row in dim.
func reasonCell(n notifs.Notification, tty bool) string {
	if !n.Unread {
		return reasonGlyph(n.Reason) + " " + reasonLabel(n.Reason)
	}
	switch n.Reason {
	case "mention", "team_mention":
		return color(ansiCyan, "@", tty) + " mention"
	case "review_requested":
		return color(ansiYellow, "◐", tty) + " review"
	case "comment":
		return color(ansiDim, "+", tty) + " comment"
	case "author", "assign":
		return color(ansiDim, "·", tty) + " own"
	}
	return color(ansiDim, "·", tty) + " " + n.Reason
}

func reasonGlyph(reason string) string {
	switch reason {
	case "mention", "team_mention":
		return "@"
	case "review_requested":
		return "◐"
	case "comment":
		return "+"
	case "author", "assign":
		return "·"
	}
	return "·"
}

func reasonLabel(reason string) string {
	switch reason {
	case "mention", "team_mention":
		return "mention"
	case "review_requested":
		return "review"
	case "comment":
		return "comment"
	case "author", "assign":
		return "own"
	}
	return reason
}

func writePRTable(ps []prs.PR, tty bool) {
	rows := make([][]string, 0, len(ps)+1)
	rows = append(rows, dimRow([]string{"CHECKS", "REVIEW", "REPO", "#", "TITLE", "BRANCH", "AGE", "LINK"}, tty))
	for _, p := range ps {
		link := p.URL
		if tty {
			link = hyperlink(p.URL, "open ↗")
		}
		rows = append(rows, []string{
			prStatusCell(p, tty),
			prReviewCell(p, tty),
			p.Repo,
			fmt.Sprintf("#%d", p.Number),
			truncate(p.Title, 40),
			truncate(p.HeadBranch, 30),
			relativeAge(p.UpdatedAt),
			link,
		})
	}
	printAligned(rows)
}

func prReviewCell(p prs.PR, tty bool) string {
	var label string
	switch p.ReviewDecision {
	case "APPROVED":
		label = color(ansiGreen, "✓", tty) + " approved"
	case "CHANGES_REQUESTED":
		label = color(ansiRed, "✗", tty) + " changes"
	case "REVIEW_REQUIRED":
		label = color(ansiDim, "·", tty) + " needs review"
	default:
		if p.ReviewCount > 0 {
			label = color(ansiYellow, "◐", tty) + " reviewed"
		} else {
			label = color(ansiDim, "·", tty) + " no review"
		}
	}
	if p.CommentCount > 0 {
		label += dim(fmt.Sprintf(" +%d", p.CommentCount), tty)
	}
	return label
}

func prStatusCell(p prs.PR, tty bool) string {
	switch {
	case p.IsFailing():
		return color(ansiRed, "✗", tty) + fmt.Sprintf(" %d/%d fail", p.Failing, p.Total)
	case p.IsPending():
		return color(ansiYellow, "◐", tty) + fmt.Sprintf(" %d/%d wait", p.Passing+p.Failing, p.Total)
	case p.IsPassing():
		return color(ansiGreen, "✓", tty) + fmt.Sprintf(" %d/%d pass", p.Passing, p.Total)
	case p.Total == 0:
		return color(ansiDim, "·", tty) + " none"
	}
	return color(ansiDim, "·", tty) + " " + p.State
}

func writeTable(rows []runs.Run, tty bool) {
	out := make([][]string, 0, len(rows)+1)
	out = append(out, dimRow([]string{"STATUS", "REPO", "WORKFLOW", "BRANCH", "AGE", "LINK"}, tty))
	for _, r := range rows {
		link := r.URL
		if tty {
			link = hyperlink(r.URL, "open ↗")
		}
		out = append(out, []string{
			statusCell(r, tty),
			r.Repo,
			truncate(r.WorkflowName, 30),
			truncate(r.Branch, 30),
			ageString(r),
			link,
		})
	}
	printAligned(out)
}

// printAligned prints rows with columns padded to their widest VISIBLE cell.
// ANSI escapes (colors, OSC 8 hyperlinks) are stripped before width measurement
// so they don't inflate column widths past what the terminal actually shows.
func printAligned(rows [][]string) {
	if len(rows) == 0 {
		return
	}
	cols := len(rows[0])
	widths := make([]int, cols)
	for _, row := range rows {
		for i := 0; i < cols && i < len(row); i++ {
			w := visibleWidth(row[i])
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	for _, row := range rows {
		for i := 0; i < cols && i < len(row); i++ {
			cell := row[i]
			fmt.Print(cell)
			if i < cols-1 {
				pad := widths[i] - visibleWidth(cell) + 2
				if pad > 0 {
					fmt.Print(strings.Repeat(" ", pad))
				}
			}
		}
		fmt.Println()
	}
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

func visibleWidth(s string) int {
	return utf8.RuneCountInString(ansiRe.ReplaceAllString(s, ""))
}

func dimRow(cells []string, tty bool) []string {
	if !tty {
		return cells
	}
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = ansiDim + c + ansiReset
	}
	return out
}

// maxVisibleRuns caps the workflow-runs section length so a busy account
// doesn't blow out the table.
const maxVisibleRuns = 10

// visibleRows filters runs for the workflow table:
//   - All currently-active runs are kept (any actor).
//   - Completed runs are kept only if triggered by the viewer themselves
//     AND within the last 24 hours.
//   - Anything else drops as soon as it finishes.
//
// Result is sorted active-first, then by UpdatedAt desc, then truncated to
// maxVisibleRuns.
func visibleRows(rs []runs.Run, viewerLogin string) []runs.Run {
	var out []runs.Run
	for _, r := range rs {
		if r.IsBot() {
			continue
		}
		switch {
		case r.IsActive():
			out = append(out, r)
		case viewerLogin != "" && r.ActorLogin == viewerLogin && time.Since(r.UpdatedAt) < 24*time.Hour:
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].IsActive(), out[j].IsActive()
		if ai != aj {
			return ai
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if len(out) > maxVisibleRuns {
		out = out[:maxVisibleRuns]
	}
	return out
}

func countByOutcome(rs []runs.Run) (active, failed int) {
	for _, r := range rs {
		if r.IsActive() {
			active++
		}
		if r.IsFailure() {
			failed++
		}
	}
	return
}

func statusCell(r runs.Run, tty bool) string {
	if r.IsActive() {
		return color(ansiYellow, "●", tty) + " " + r.Status
	}
	switch r.Conclusion {
	case "success":
		return color(ansiGreen, "✓", tty) + " success"
	case "failure", "timed_out", "startup_failure":
		return color(ansiRed, "✗", tty) + " " + r.Conclusion
	case "cancelled", "skipped":
		return color(ansiDim, "·", tty) + " " + r.Conclusion
	}
	return r.Conclusion
}

func ageString(r runs.Run) string {
	t := r.UpdatedAt
	if r.IsActive() {
		t = r.CreatedAt
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

func hyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

func setWindowTitle(s string) {
	fmt.Fprintf(os.Stderr, "\x1b]2;%s\x07", s)
}

// ClearWindowTitle restores the terminal title to an empty string. Call on
// exit so the window doesn't keep showing a stale status.
func ClearWindowTitle() {
	if !isTTY(os.Stderr) {
		return
	}
	fmt.Fprint(os.Stderr, "\x1b]2;\x07")
}

// EnterAltScreen swaps the terminal to its alternate screen buffer, puts stdin
// in cbreak mode (-echo -icanon) so keypresses arrive byte-by-byte without
// being echoed, and hides the cursor. Returns an opaque token to pass to
// ExitAltScreen to restore the previous terminal state exactly.
func EnterAltScreen() (saved string) {
	if !isTTY(os.Stdout) {
		return ""
	}
	saved = sttySaveState()
	sttyApply("-echo", "-icanon")
	fmt.Fprint(os.Stdout, "\x1b[?1049h\x1b[H\x1b[?25l")
	return saved
}

// ExitAltScreen restores the primary screen buffer, restores the cursor, and
// restores the terminal mode captured by EnterAltScreen.
func ExitAltScreen(saved string) {
	if !isTTY(os.Stdout) {
		return
	}
	fmt.Fprint(os.Stdout, "\x1b[?25h\x1b[?1049l")
	sttyRestore(saved)
}

func sttySaveState() string {
	if !isTTY(os.Stdin) {
		return ""
	}
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func sttyRestore(saved string) {
	if saved == "" || !isTTY(os.Stdin) {
		return
	}
	cmd := exec.Command("stty", saved)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

func sttyApply(args ...string) {
	if !isTTY(os.Stdin) {
		return
	}
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

// SpinnerFrame returns the spinner glyph for the given frame index.
func SpinnerFrame(frame int) string {
	const frames = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	runes := []rune(frames)
	return string(runes[frame%len(runes)])
}

// RenderSpinner overwrites the header line in place with an animated
// spinner. Cheap (one terminal write, no full redraw) so it's safe to call
// at ~8fps. No-op on non-tty.
func RenderSpinner(frame int) {
	if !isTTY(os.Stdout) {
		return
	}
	fmt.Fprintf(os.Stdout,
		"\x1b[1;1H\x1b[K%sgha-monitor%s%s · %s refreshing%s",
		ansiBold, ansiReset,
		ansiDim, SpinnerFrame(frame), ansiReset,
	)
}

func color(code, s string, tty bool) string {
	if !tty {
		return s
	}
	return code + s + ansiReset
}

func dim(s string, tty bool) string {
	if !tty {
		return s
	}
	return ansiDim + s + ansiReset
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
