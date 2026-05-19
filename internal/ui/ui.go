package ui

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/georgearnall/gha-monitor/internal/notifs"
	"github.com/georgearnall/gha-monitor/internal/prs"
	"github.com/georgearnall/gha-monitor/internal/runs"
)

const (
	// ansiHome moves the cursor to (1,1) without clearing. The render uses
	// it together with ansiClearBelow so each redraw overwrites content in
	// place (no full-screen-clear flash on every keypress).
	ansiHome       = "\x1b[H"
	ansiClearBelow = "\x1b[J"
	ansiReset      = "\x1b[0m"
	ansiBold       = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiGreen  = "\x1b[32m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

type Snapshot struct {
	Runs           []runs.Run
	PRs            []prs.PR
	Notifs         []notifs.Notification
	FocusedNotifID string // notification ID to highlight, or "" for none
	FocusedPRKey   string // PR key "owner/repo#number" to highlight, or "" for none
	FocusedRunID   string // run ID (as string) to highlight, or "" for none
	ViewerLogin    string // authenticated user's login, used to keep their own runs longer
	RepoCount      int
	RateRemaining  int
	RateLimit      int
	PolledAt       time.Time
	NextPollIn     time.Duration
	TermWidth      int  // 0 = unknown / unconstrained
	Stale          bool // rendering from disk cache, not fresh
	Refreshing     bool // a background refresh is in flight
}

// Render redraws the status table. Safe to call when stdout is not a tty;
// ANSI control codes are suppressed in that case.
func Render(snap Snapshot) {
	tty := isTTY(os.Stdout)

	rows := VisibleRows(snap.Runs, snap.ViewerLogin)
	active, failed := countByOutcome(rows)
	notifRows := VisibleNotifs(snap.Notifs)
	unread := unreadCount(notifRows)

	if tty {
		fmt.Print(ansiHome)
		setWindowTitle(windowTitleString(unread, active, failed))
	}

	header(snap, tty)

	if len(notifRows) > 0 {
		fmt.Println()
		fmt.Println(dim("NOTIFICATIONS", tty))
		writeNotifsTable(notifRows, snap.FocusedNotifID, tty, snap.TermWidth)
	}

	if len(snap.PRs) > 0 {
		fmt.Println()
		fmt.Println(dim("PULL REQUESTS", tty))
		writePRTable(snap.PRs, snap.FocusedPRKey, tty, snap.TermWidth)
	}

	if len(rows) > 0 || (len(snap.PRs) == 0 && len(notifRows) == 0) {
		fmt.Println()
		if len(snap.PRs) > 0 || len(notifRows) > 0 {
			fmt.Println(dim("WORKFLOW RUNS", tty))
		}
		if len(rows) == 0 {
			fmt.Println(dim("no active runs or recent failures", tty))
		} else {
			writeTable(rows, snap.FocusedRunID, tty, snap.TermWidth)
		}
	}
	footer(snap, tty)
	if tty {
		// Clear from the current cursor position to the end of the screen.
		// Removes any leftover lines from a previous render that was taller.
		fmt.Print(ansiClearBelow)
	}
}

func windowTitleString(unread, active, failed int) string {
	if unread > 0 {
		return fmt.Sprintf("gha-monitor · %d unread · %d active · %d recent failures", unread, active, failed)
	}
	return fmt.Sprintf("gha-monitor · %d active · %d recent failures", active, failed)
}

func header(_ Snapshot, tty bool) {
	title := "gha-monitor"
	if tty {
		title = ansiBold + title + ansiReset
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
		fmt.Println(dim("[↑↓] move  [↵] open  [m] mark read  [M] mark all  [r] refresh  [q] quit", tty))
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

// VisibleNotifs slices the notifications list to maxVisibleNotifs. Filtering
// and sorting already happened upstream in notifs.Poll. Exported so the
// watch loop can build a cursor target list that exactly matches what's
// rendered (e.g. for arrow-key navigation).
func VisibleNotifs(ns []notifs.Notification) []notifs.Notification {
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

// notifsRepoCol is the column index of REPO inside writeNotifsTable's rows.
// Kept as a const so fitRepoColumn can target it directly.
const notifsRepoCol = 3

func writeNotifsTable(ns []notifs.Notification, focusedID string, tty bool, termWidth int) {
	rows := make([][]string, 0, len(ns)+1)
	rows = append(rows, dimRow([]string{"  ", "REASON", "TITLE", "REPO", "#", "AGE", "LINK"}, tty))
	dimMask := make([]bool, len(ns)) // which data row should be dimmed post-fit
	for i, n := range ns {
		link := n.URL
		if tty {
			link = hyperlink(n.URL, "open ↗")
		}
		cursor := "  "
		if n.ID == focusedID {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		rows = append(rows, []string{
			cursor,
			reasonCell(n, tty),
			truncate(n.Title, 50),
			n.Repo, // kept plain so shrinkRepo can parse the slash
			fmt.Sprintf("#%d", n.PRNumber),
			relativeAge(n.UpdatedAt),
			link,
		})
		if !n.Unread && tty {
			dimMask[i] = true
		}
	}
	fitRepoColumn(rows, notifsRepoCol, termWidth)
	// Apply per-row dimming AFTER shrinking so shrinkRepo sees raw repo
	// strings. Cursor (col 0) stays bright on dimmed rows.
	for i, d := range dimMask {
		if !d {
			continue
		}
		for j := 1; j < len(rows[i+1]); j++ {
			rows[i+1][j] = ansiDim + rows[i+1][j] + ansiReset
		}
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

// prRepoCol is the column index of REPO inside writePRTable's rows.
const prRepoCol = 4

func writePRTable(ps []prs.PR, focusedKey string, tty bool, termWidth int) {
	rows := make([][]string, 0, len(ps)+1)
	rows = append(rows, dimRow([]string{"  ", "CHECKS", "REVIEW", "TITLE", "REPO", "#", "BRANCH", "AGE", "LINK"}, tty))
	for _, p := range ps {
		link := p.URL
		if tty {
			link = hyperlink(p.URL, "open ↗")
		}
		cursor := "  "
		if focusedKey != "" && fmt.Sprintf("%s#%d", p.Repo, p.Number) == focusedKey {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		rows = append(rows, []string{
			cursor,
			prStatusCell(p, tty),
			prReviewCell(p, tty),
			truncate(p.Title, 40),
			p.Repo,
			fmt.Sprintf("#%d", p.Number),
			truncate(p.HeadBranch, 30),
			relativeAge(p.UpdatedAt),
			link,
		})
	}
	fitRepoColumn(rows, prRepoCol, termWidth)
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

// runsRepoCol is the column index of REPO inside writeTable's rows.
const runsRepoCol = 3

func writeTable(rows []runs.Run, focusedID string, tty bool, termWidth int) {
	out := make([][]string, 0, len(rows)+1)
	out = append(out, dimRow([]string{"  ", "STATUS", "WORKFLOW", "REPO", "BRANCH", "AGE", "LINK"}, tty))
	for _, r := range rows {
		link := r.URL
		if tty {
			link = hyperlink(r.URL, "open ↗")
		}
		cursor := "  "
		if focusedID != "" && strconv.FormatInt(r.ID, 10) == focusedID {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		out = append(out, []string{
			cursor,
			statusCell(r, tty),
			truncate(r.WorkflowName, 30),
			r.Repo,
			truncate(r.Branch, 30),
			ageString(r),
			link,
		})
	}
	fitRepoColumn(out, runsRepoCol, termWidth)
	printAligned(out)
}

// TermWidth returns the terminal's column count by shelling out to
// `stty size`. Returns 0 when stdin/stdout isn't a tty or stty fails
// (caller treats 0 as "unconstrained, don't shrink").
func TermWidth() int {
	if !isTTY(os.Stdout) || !isTTY(os.Stdin) {
		return 0
	}
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0
	}
	cols, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return cols
}

// fitRepoColumn shrinks the cells at colIdx so the total natural width of
// every row fits within termWidth. The repo cell is shrunk owner-first
// (owner part replaced with leading ellipsis), preserving the full repo
// name when possible. No-op when termWidth <= 0 or the table already fits.
func fitRepoColumn(rows [][]string, colIdx, termWidth int) {
	if termWidth <= 0 || len(rows) == 0 {
		return
	}
	cols := len(rows[0])
	if colIdx < 0 || colIdx >= cols {
		return
	}
	const gap = 2 // matches the inter-column padding in printAligned

	// Compute per-column widths (visible).
	widths := make([]int, cols)
	for _, row := range rows {
		for i := 0; i < cols && i < len(row); i++ {
			w := visibleWidth(row[i])
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	total := 0
	for i, w := range widths {
		total += w
		if i < cols-1 {
			total += gap
		}
	}
	if total <= termWidth {
		return
	}

	// Budget for the repo column: shrink it (and only it) to fit.
	others := total - widths[colIdx]
	budget := termWidth - others
	if budget < 4 {
		budget = 4 // never shrink to less than "x/y" + ellipsis fallback
	}
	for r := 1; r < len(rows); r++ {
		if colIdx >= len(rows[r]) {
			continue
		}
		rows[r][colIdx] = shrinkRepo(rows[r][colIdx], budget)
	}
}

// shrinkRepo fits "owner/name" into budget visible runes. Prefers to keep
// the full repo name and trim the owner with a leading ellipsis. Falls
// back to a generic truncate when even the name can't fit.
func shrinkRepo(repo string, budget int) string {
	if visibleWidth(repo) <= budget {
		return repo
	}
	slash := strings.IndexByte(repo, '/')
	if slash < 0 {
		return truncate(repo, budget)
	}
	name := repo[slash:] // includes the slash
	nameLen := visibleWidth(name)
	ownerBudget := budget - nameLen
	if ownerBudget < 2 {
		// not enough room for at least "x…/name" — fall back to generic
		// truncation of the whole string
		return truncate(repo, budget)
	}
	ownerRunes := []rune(repo[:slash])
	if len(ownerRunes) <= ownerBudget {
		return repo
	}
	return string(ownerRunes[:ownerBudget-1]) + "…" + name
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

// VisibleRows filters runs for the workflow table:
//   - All currently-active runs are kept (any actor).
//   - Completed runs are kept only if triggered by the viewer themselves
//     AND within the last 24 hours.
//   - Anything else drops as soon as it finishes.
//
// Result is sorted active-first, then by UpdatedAt desc, then truncated to
// maxVisibleRuns. Exported so the watch loop can mirror the same filter
// when building its cursor target list.
func VisibleRows(rs []runs.Run, viewerLogin string) []runs.Run {
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
