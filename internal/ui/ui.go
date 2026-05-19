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
	// ansiClearEOL clears from the cursor to the end of the current line.
	// Appended to every printed line during in-place redraws so leftover
	// characters from a previous, longer line don't dangle.
	ansiClearEOL = "\x1b[K"
	ansiReset      = "\x1b[0m"
	ansiBold       = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiGreen  = "\x1b[32m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	// ansiPaleBlue is a 256-colour soft blue used for hyperlink labels,
	// distinct from any of the status colours.
	ansiPaleBlue = "\x1b[38;5;111m"
	// ansiAmber is a 256-colour muted gold used to subtly distinguish
	// ticket references like [ECOM-9026], NB-1068, etc. in titles.
	ansiAmber = "\x1b[38;5;179m"
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

	pln(tty)
	pln(tty, dim("NOTIFICATIONS", tty))
	if len(notifRows) > 0 {
		writeNotifsTable(notifRows, snap.FocusedNotifID, tty, snap.TermWidth)
	} else {
		pln(tty, dim("all caught up", tty))
	}

	pln(tty)
	pln(tty, dim("PULL REQUESTS", tty))
	if len(snap.PRs) > 0 {
		writePRTable(snap.PRs, snap.FocusedPRKey, tty, snap.TermWidth)
	} else {
		pln(tty, dim("no open pull requests", tty))
	}

	pln(tty)
	pln(tty, dim("WORKFLOW RUNS", tty))
	if len(rows) > 0 {
		writeTable(rows, snap.FocusedRunID, tty, snap.TermWidth)
	} else {
		pln(tty, dim("no active runs or recent failures", tty))
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
	pln(tty, title)
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
	pln(tty)
	pln(tty, dim(join(parts, " · "), tty))
	if tty {
		pln(tty, dim("[↑↓] move  [↵] open  [m] read  [d] dismiss  [M] read all  [r] refresh  [q] quit", tty))
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
		title := truncate(n.Title, 50)
		if tty {
			if n.Unread {
				link = coloredHyperlink(n.URL, "open ↗")
				title = styleTickets(title)
			} else {
				// Read row: avoid inner SGR codes that would break the
				// outer dim wrap applied below.
				link = hyperlink(n.URL, "open ↗")
			}
		}
		cursor := "  "
		if n.ID == focusedID {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		rows = append(rows, []string{
			cursor,
			reasonCell(n, tty),
			title,
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
	// strings. Cursor (col 0) stays bright on dimmed rows. Unread rows
	// also get the owner-dim treatment so the repo name pops.
	for i, d := range dimMask {
		if d {
			for j := 1; j < len(rows[i+1]); j++ {
				rows[i+1][j] = ansiDim + rows[i+1][j] + ansiReset
			}
			continue
		}
		rows[i+1][notifsRepoCol] = styleRepoCell(rows[i+1][notifsRepoCol], tty)
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
// Same value with or without BRANCH (BRANCH sits after REPO).
const prRepoCol = 4

// prTitleMax is the upper bound on title length before truncation.
const prTitleMax = 55

func writePRTable(ps []prs.PR, focusedKey string, tty bool, termWidth int) {
	// Adaptive fit: try full layout first; if it overflows, drop BRANCH;
	// if it still overflows, also drop the trailing "pass"/"fail"/"wait"
	// status word. The repo column is shrunk last by fitRepoColumn.
	rows := buildPRRows(ps, focusedKey, tty, true /*branch*/, false /*compact status*/)
	if termWidth > 0 && tableWidth(rows) > termWidth {
		rows = buildPRRows(ps, focusedKey, tty, false, false)
		if tableWidth(rows) > termWidth {
			rows = buildPRRows(ps, focusedKey, tty, false, true)
		}
	}
	fitRepoColumn(rows, prRepoCol, termWidth)
	for r := 1; r < len(rows); r++ {
		if prRepoCol < len(rows[r]) {
			rows[r][prRepoCol] = styleRepoCell(rows[r][prRepoCol], tty)
		}
	}
	printAligned(rows)
}

func buildPRRows(ps []prs.PR, focusedKey string, tty, includeBranch, compactStatus bool) [][]string {
	header := []string{"  ", "CHECKS", "REVIEW", "TITLE", "REPO", "#"}
	if includeBranch {
		header = append(header, "BRANCH")
	}
	header = append(header, "AGE", "LINK")

	rows := make([][]string, 0, len(ps)+1)
	rows = append(rows, dimRow(header, tty))
	for _, p := range ps {
		link := p.URL
		title := truncate(p.Title, prTitleMax)
		if tty {
			link = coloredHyperlink(p.URL, "open ↗")
			title = styleTickets(title)
		}
		cursor := "  "
		if focusedKey != "" && fmt.Sprintf("%s#%d", p.Repo, p.Number) == focusedKey {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		row := []string{
			cursor,
			prStatusCell(p, tty, compactStatus),
			prReviewCell(p, tty),
			title,
			p.Repo,
			fmt.Sprintf("#%d", p.Number),
		}
		if includeBranch {
			row = append(row, truncate(p.HeadBranch, 30))
		}
		row = append(row, relativeAge(p.UpdatedAt), link)
		rows = append(rows, row)
	}
	return rows
}

func prReviewCell(p prs.PR, tty bool) string {
	var label string
	switch p.ReviewDecision {
	case "APPROVED":
		label = color(ansiGreen, "✓", tty) + " approved"
	case "CHANGES_REQUESTED":
		label = color(ansiRed, "✗", tty) + " changes"
	case "REVIEW_REQUIRED":
		label = color(ansiDim, "·", tty) + " blocked"
	default:
		if p.ReviewCount > 0 {
			label = color(ansiYellow, "◐", tty) + " reviewed"
		} else {
			// No decision and no reviewers yet: render nothing rather
			// than a "no review" placeholder that just wastes column.
			label = ""
		}
	}
	if p.CommentCount > 0 {
		if label != "" {
			label += " "
		}
		label += dim(fmt.Sprintf("+%d", p.CommentCount), tty)
	}
	return label
}

func prStatusCell(p prs.PR, tty, compact bool) string {
	failed := func() string {
		if compact {
			return fmt.Sprintf(" %d/%d", p.Failing, p.Total)
		}
		return fmt.Sprintf(" %d/%d fail", p.Failing, p.Total)
	}
	pending := func() string {
		if compact {
			return fmt.Sprintf(" %d/%d", p.Passing+p.Failing, p.Total)
		}
		return fmt.Sprintf(" %d/%d wait", p.Passing+p.Failing, p.Total)
	}
	passed := func() string {
		if compact {
			return fmt.Sprintf(" %d/%d", p.Passing, p.Total)
		}
		return fmt.Sprintf(" %d/%d pass", p.Passing, p.Total)
	}

	switch {
	case p.IsFailing():
		return color(ansiRed, "✗", tty) + failed()
	case p.IsPending():
		return color(ansiYellow, "◐", tty) + pending()
	case p.IsPassing():
		return color(ansiGreen, "✓", tty) + passed()
	case p.Total == 0:
		if compact {
			return color(ansiDim, "·", tty)
		}
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
			link = coloredHyperlink(r.URL, "open ↗")
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
	for r := 1; r < len(out); r++ {
		if runsRepoCol < len(out[r]) {
			out[r][runsRepoCol] = styleRepoCell(out[r][runsRepoCol], tty)
		}
	}
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

// columnWidths returns the max visible width per column across all rows.
func columnWidths(rows [][]string) []int {
	if len(rows) == 0 {
		return nil
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
	return widths
}

// tableWidth returns the total natural rendered width of rows (per-column
// max widths plus inter-column gaps), matching what printAligned will emit.
func tableWidth(rows [][]string) int {
	widths := columnWidths(rows)
	if len(widths) == 0 {
		return 0
	}
	total := 0
	for i, w := range widths {
		total += w
		if i < len(widths)-1 {
			total += 2 // gap matches printAligned
		}
	}
	return total
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
	widths := columnWidths(rows)
	total := tableWidth(rows)
	if total <= termWidth {
		return
	}
	others := total - widths[colIdx]
	budget := termWidth - others
	if budget < 4 {
		budget = 4
	}
	for r := 1; r < len(rows); r++ {
		if colIdx >= len(rows[r]) {
			continue
		}
		rows[r][colIdx] = shrinkRepo(rows[r][colIdx], budget)
	}
}

// styleRepoCell dims the organisation portion (owner + slash) so the repo
// name stands out. No-op on non-tty or when the cell has no slash (i.e.
// shrinkRepo already dropped the org).
//
// CALLER NOTE: must not be applied to cells that will be wrapped in an
// outer dim later; the embedded reset would terminate the outer dim wrap.
func styleRepoCell(cell string, tty bool) string {
	if !tty {
		return cell
	}
	slash := strings.IndexByte(cell, '/')
	if slash < 0 {
		return cell
	}
	return ansiDim + cell[:slash+1] + ansiReset + cell[slash+1:]
}

// shrinkRepo fits "owner/name" into budget visible runes. Cascade:
//  1. fits as-is → return unchanged
//  2. enough room for at least one owner char + "…/name" → trim owner
//  3. not enough room for any owner stub → drop the org entirely, show
//     just the repo name (truncated if still too long)
//
// The "show the repo name" goal beats showing a long org prefix, because
// when a user has many notifications for one org the org name is repeated
// noise but the repo name is the distinguishing signal.
func shrinkRepo(repo string, budget int) string {
	if visibleWidth(repo) <= budget {
		return repo
	}
	slash := strings.IndexByte(repo, '/')
	if slash < 0 {
		return truncate(repo, budget)
	}
	name := repo[slash+1:] // without the slash
	nameLen := visibleWidth(name)
	ownerRunes := []rune(repo[:slash])

	// Try preserving some of the owner: "<stub>…/<name>". Width consumed
	// is stub + 2 (ellipsis + slash) + nameLen, so stub = budget-2-nameLen.
	ownerStub := budget - 2 - nameLen
	if ownerStub >= 1 && ownerStub < len(ownerRunes) {
		return string(ownerRunes[:ownerStub]) + "…/" + name
	}

	// No room for a useful owner stub: drop the org. Prefer full name,
	// fall back to truncated name.
	if nameLen <= budget {
		return name
	}
	return truncate(name, budget)
}

// printAligned prints rows with columns padded to their widest VISIBLE cell.
// ANSI escapes (colors, OSC 8 hyperlinks) are stripped before width measurement
// so they don't inflate column widths past what the terminal actually shows.
// Each output line is terminated with a clear-to-EOL when stdout is a tty,
// so an in-place redraw doesn't leave dangling characters from a previous
// longer line.
func printAligned(rows [][]string) {
	if len(rows) == 0 {
		return
	}
	tty := isTTY(os.Stdout)
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
	var line strings.Builder
	for _, row := range rows {
		line.Reset()
		for i := 0; i < cols && i < len(row); i++ {
			cell := row[i]
			line.WriteString(cell)
			if i < cols-1 {
				pad := widths[i] - visibleWidth(cell) + 2
				if pad > 0 {
					line.WriteString(strings.Repeat(" ", pad))
				}
			}
		}
		pln(tty, line.String())
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

// hyperlink emits an OSC 8 hyperlink with no colour styling. Safe to use
// inside an outer ansiDim wrap because OSC sequences don't carry SGR
// attributes that would interact with intensity.
func hyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// coloredHyperlink emits a pale-blue OSC 8 hyperlink. The inner SGR reset
// would terminate any outer dim wrap, so callers must NOT use this for
// cells that will be dimmed at the row level (use plain hyperlink there).
func coloredHyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + ansiPaleBlue + text + ansiReset + "\x1b]8;;\x1b\\"
}

// ticketRe matches things that look like ticket references: an optional
// opening bracket, 2+ uppercase letters, a hyphen, one or more digits,
// and an optional closing bracket. Covers [ECOM-9026], NB-1068, etc.
var ticketRe = regexp.MustCompile(`\[?[A-Z]{2,}-\d+\]?`)

// styleTickets wraps every ticket-like substring in ansiAmber so they're
// visually distinct in a title. Same dim-wrap caveat as coloredHyperlink.
func styleTickets(s string) string {
	return ticketRe.ReplaceAllStringFunc(s, func(match string) string {
		return ansiAmber + match + ansiReset
	})
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

// pln writes a line followed by a clear-to-EOL escape (on tty) and a
// newline. Use instead of fmt.Println inside Render so a shorter new
// line doesn't leave the trailing characters of a longer previous render
// on screen.
func pln(tty bool, args ...any) {
	if tty {
		fmt.Print(fmt.Sprint(args...) + ansiClearEOL + "\n")
		return
	}
	fmt.Println(args...)
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
