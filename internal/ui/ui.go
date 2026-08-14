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

	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
	"golang.org/x/term"
)

const runsWindow = 48 * time.Hour

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
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiDim      = "\x1b[2m"
	ansiGreen    = "\x1b[32m"
	ansiRed      = "\x1b[31m"
	ansiYellow   = "\x1b[33m"
	ansiCyan     = "\x1b[36m"
	// ansiPaleBlue is a 256-colour soft blue used for hyperlink labels,
	// distinct from any of the status colours.
	ansiPaleBlue = "\x1b[38;5;111m"
	// ansiAmber is a 256-colour muted gold used to subtly distinguish
	// ticket references like [ECOM-9026], NB-1068, etc. in titles.
	ansiAmber = "\x1b[38;5;179m"
	// ansiPurple is a 256-colour purple chosen to match GitHub's merged-
	// PR colour reasonably well in most terminals.
	ansiPurple = "\x1b[38;5;141m"
	// ansiDefaultFg cancels the foreground colour without touching the
	// intensity attribute. Used as a closer inside row-level dim wraps
	// so that a colour span ends without also turning off dim.
	ansiDefaultFg = "\x1b[39m"
)

// Cell is a sequence of plain-text or colour-coded fragments. Render picks
// the right colour closer based on whether the row will be wrapped in an
// outer dim later, so callers no longer have to choose between color() and
// colorInsideDim() when building cell content.
type Cell struct {
	parts []cellPart
}

type cellPart struct {
	text  string
	color string // empty == plain text
}

func NewCell() *Cell                 { return &Cell{} }
func (c *Cell) Plain(s string) *Cell { c.parts = append(c.parts, cellPart{text: s}); return c }
func (c *Cell) Colored(code, s string) *Cell {
	c.parts = append(c.parts, cellPart{text: s, color: code})
	return c
}

// Render returns the ANSI-encoded string for this cell. insideDim picks the
// dim-safe closer (\x1b[39m) so an outer ansiDim wrap survives the span end.
// When tty is false, all colour codes are skipped.
func (c *Cell) Render(tty, insideDim bool) string {
	var b strings.Builder
	for _, p := range c.parts {
		if !tty || p.color == "" {
			b.WriteString(p.text)
			continue
		}
		if insideDim {
			b.WriteString(colorInsideDim(p.color, p.text, true))
		} else {
			b.WriteString(color(p.color, p.text, true))
		}
	}
	return b.String()
}

// tableRow is one data row in a rendered panel. The repo column must be plain
// text (no ANSI) so fitRepoColumn can measure and shrink it before styling.
type tableRow struct {
	cells []string
	dim   bool // if true: wrap cells[1:] in ansiDim+ansiReset post-fit; skip styleRepoCell
}

// panelTable is the shared rendering pipeline for all three panels:
//  1. fitRepoColumn (if repoColIdx >= 0)
//  2. for each data row: either outer-dim all non-cursor cells, or styleRepoCell
//  3. printAligned
type panelTable struct {
	headers    []string // already dim-styled via dimRow()
	rows       []tableRow
	repoColIdx int // -1 if no repo column
	termWidth  int
	tty        bool
}

func newPanelTable(headers []string, repoColIdx, termWidth int, tty bool) *panelTable {
	return &panelTable{headers: headers, repoColIdx: repoColIdx, termWidth: termWidth, tty: tty}
}

func (t *panelTable) addRow(cells []string, dim bool) {
	t.rows = append(t.rows, tableRow{cells: cells, dim: dim})
}

func (t *panelTable) render() {
	raw := make([][]string, 0, len(t.rows)+1)
	raw = append(raw, t.headers)
	for _, r := range t.rows {
		raw = append(raw, r.cells)
	}
	if t.repoColIdx >= 0 {
		fitRepoColumn(raw, t.repoColIdx, t.termWidth)
	}
	for i, r := range t.rows {
		if r.dim {
			for j := 1; j < len(raw[i+1]); j++ {
				raw[i+1][j] = ansiDim + raw[i+1][j] + ansiReset
			}
		} else if t.repoColIdx >= 0 && t.repoColIdx < len(raw[i+1]) {
			raw[i+1][t.repoColIdx] = styleRepoCell(raw[i+1][t.repoColIdx], t.tty)
		}
	}
	printAligned(raw)
}

type Snapshot struct {
	Runs           []runs.Run
	PRs            []prs.PR
	AssignedPRs    []prs.PR
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
	TermWidth      int    // 0 = unknown / unconstrained
	Stale          bool   // rendering from disk cache, not fresh
	Refreshing     bool   // a background refresh is in flight
	BgErr          string // recent background-task error to surface in the footer
	JiraURL        string // base URL for clickable ticket refs (empty = no links)
	PromptLine     string // non-empty: show an inline input prompt at the footer
	Links          bool             // whether the terminal supports OSC 8 hyperlinks; hides LINK column when false
	ReadRunIDs     map[int64]bool   // run IDs the user has marked read; rendered dimmed
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
		writeNotifsTable(notifRows, snap.FocusedNotifID, tty, snap.TermWidth, snap.JiraURL, snap.Links)
	} else {
		pln(tty, dim("all caught up", tty))
	}

	pln(tty)
	pln(tty, dim("PULL REQUESTS", tty))
	if len(snap.PRs) > 0 {
		writePRTable(snap.PRs, snap.FocusedPRKey, tty, snap.TermWidth, snap.JiraURL, snap.Links)
	} else {
		pln(tty, dim("no open pull requests", tty))
	}

	if len(snap.AssignedPRs) > 0 {
		pln(tty)
		pln(tty, dim("ASSIGNED PULL REQUESTS", tty))
		writePRTable(snap.AssignedPRs, snap.FocusedPRKey, tty, snap.TermWidth, snap.JiraURL, snap.Links)
	}

	pln(tty)
	pln(tty, dim("WORKFLOW RUNS", tty))
	if len(rows) > 0 {
		writeTable(rows, snap.FocusedRunID, tty, snap.TermWidth, snap.Links, snap.ReadRunIDs)
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

// RepoStatus is a flattened view of one discovered repository for the config
// screen. Callers build it from state.Repos + state.MutedRepos.
type RepoStatus struct {
	Name     string    // "owner/repo"
	Activity time.Time
	Muted    bool
}

// ConfigSnapshot is the input to RenderConfig.
type ConfigSnapshot struct {
	Repos          []RepoStatus
	ExcludedRepos  []string // sorted, from --exclude flag
	ViewerLogin    string
	BaseInterval   time.Duration
	ActiveInterval time.Duration
	RepoRefresh    time.Duration
	MaxRepos       int
	PRSince        time.Duration
	RateRemaining  int
	RateLimit      int
	JiraURL        string
	TermWidth      int
}

// RenderConfig redraws the config screen, which entirely replaces the main
// view while active. Shows settings, monitored repos with muted status, and
// any statically excluded repos.
func RenderConfig(snap ConfigSnapshot) {
	tty := isTTY(os.Stdout)
	if tty {
		fmt.Print(ansiHome)
	}

	title := "gh-monitor"
	if tty {
		title = ansiBold + title + ansiReset
	}
	pln(tty, title+" - config")

	pln(tty)
	pln(tty, dim("SETTINGS", tty))
	jiraURLVal := snap.JiraURL
	if jiraURLVal == "" {
		jiraURLVal = dim("not configured", tty)
	}
	settings := [][]string{
		{"  " + dim("Active poll", tty), snap.ActiveInterval.String()},
		{"  " + dim("Idle poll", tty), snap.BaseInterval.String()},
		{"  " + dim("Repo refresh", tty), snap.RepoRefresh.String()},
		{"  " + dim("PR window", tty), prSinceLabel(snap.PRSince)},
		{"  " + dim("Max repos", tty), strconv.Itoa(snap.MaxRepos)},
		{"  " + dim("Viewer", tty), snap.ViewerLogin},
		{"  " + dim("Jira URL", tty), jiraURLVal},
	}
	if snap.RateLimit > 0 {
		settings = append(settings, []string{"  " + dim("Rate limit", tty), fmt.Sprintf("%d/%d", snap.RateRemaining, snap.RateLimit)})
	}
	printAligned(settings)

	pln(tty)
	pln(tty, dim(fmt.Sprintf("MONITORED REPOS (%d)", len(snap.Repos)), tty))
	if len(snap.Repos) == 0 {
		pln(tty, "  "+dim("none discovered yet", tty))
	} else {
		writeConfigRepoTable(snap.Repos, tty, snap.TermWidth)
	}

	if len(snap.ExcludedRepos) > 0 {
		pln(tty)
		pln(tty, dim(fmt.Sprintf("EXCLUDED REPOS (%d)", len(snap.ExcludedRepos)), tty))
		for _, r := range snap.ExcludedRepos {
			pln(tty, "  "+r)
		}
	}

	pln(tty)
	if tty {
		pln(tty, dim("[?] close  [q] quit", tty))
	}
	if tty {
		fmt.Print(ansiClearBelow)
	}
}

func prSinceLabel(d time.Duration) string {
	if d == 0 {
		return "disabled"
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return d.String()
}

func writeConfigRepoTable(repos []RepoStatus, tty bool, termWidth int) {
	t := newPanelTable(
		dimRow([]string{"REPO", "LAST ACTIVITY", "STATUS"}, tty),
		0, termWidth, tty,
	)
	for _, r := range repos {
		status := ""
		if r.Muted {
			status = dim("muted", tty)
		}
		t.addRow([]string{r.Name, relativeAge(r.Activity), status}, false)
	}
	t.render()
}

func windowTitleString(unread, active, failed int) string {
	if unread > 0 {
		return fmt.Sprintf("gh-monitor · %d unread · %d active · %d recent failures", unread, active, failed)
	}
	return fmt.Sprintf("gh-monitor · %d active · %d recent failures", active, failed)
}

func header(_ Snapshot, tty bool) {
	title := "gh-monitor"
	if tty {
		title = ansiBold + title + ansiReset
	}
	pln(tty, title)
}

func footer(snap Snapshot, tty bool) {
	parts := []string{polledLabel(snap)}
	if len(snap.Notifs) > 0 {
		parts = append(parts, fmt.Sprintf("%d notifs", len(snap.Notifs)))
	}
	if len(snap.PRs) > 0 {
		parts = append(parts, fmt.Sprintf("%d PRs", len(snap.PRs)))
	}
	if snap.Refreshing {
		parts = append(parts, "refreshing…")
	} else if snap.NextPollIn > 0 {
		parts = append(parts, fmt.Sprintf("next poll in %s", snap.NextPollIn.Round(time.Second)))
	}
	pln(tty)
	if tty {
		pln(tty, dim("[↑↓] move  [↵] open  [m] read  [d] dismiss  [x] mute repo  [t] ticket  [r] refresh  [?] config  [q] quit", tty))
	}
	pln(tty, dim(join(parts, " · "), tty))
	if snap.BgErr != "" {
		pln(tty, color(ansiRed, "⚠ "+snap.BgErr, tty))
	}
	if snap.PromptLine != "" {
		pln(tty, snap.PromptLine)
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

func writeNotifsTable(ns []notifs.Notification, focusedID string, tty bool, termWidth int, jiraURL string, links bool) {
	headers := []string{"  ", "REASON", "TITLE", "REPO", "#", "AGE"}
	if links {
		headers = append(headers, "LINK")
	}
	t := newPanelTable(dimRow(headers, tty), notifsRepoCol, termWidth, tty)
	for _, n := range ns {
		title := truncate(n.Title, 50)
		if tty && n.Unread {
			title = styleTickets(title, jiraURL)
		}
		cursor := "  "
		if n.ID == focusedID {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		row := []string{
			cursor,
			reasonCell(n).Render(tty, !n.Unread),
			title,
			n.Repo, // plain so shrinkRepo can parse the slash
			fmt.Sprintf("#%d", n.PRNumber),
			relativeAge(n.UpdatedAt),
		}
		if links {
			var link string
			if n.Unread {
				link = coloredHyperlink(n.URL, "open ↗")
			} else {
				// Read row: avoid inner SGR codes that would break the
				// outer dim wrap applied by panelTable.render().
				link = hyperlink(n.URL, "open ↗")
			}
			row = append(row, link)
		}
		t.addRow(row, !n.Unread && tty)
	}
	t.render()
}

// reasonCell builds a Cell for the notification reason column. The Render
// call at the use site supplies tty and insideDim (= !n.Unread) so the
// colour closer is chosen there rather than baked in here.
func reasonCell(n notifs.Notification) *Cell {
	if n.Reason == "author" || n.Reason == "assign" {
		return stateCell(n.PRState)
	}
	// For terminal states, lead with the state so the user knows the PR is done.
	if n.PRState == notifs.PRStateMerged || n.PRState == notifs.PRStateClosed {
		icon, label, col := stateGlyph(n.PRState)
		return NewCell().Colored(col, icon).Plain(" " + label)
	}
	c := NewCell()
	switch n.Reason {
	case "mention", "team_mention":
		return c.Colored(ansiCyan, "@").Plain(" mention")
	case "review_requested":
		return c.Colored(ansiYellow, "◐").Plain(" review")
	case "comment":
		return c.Colored(ansiDim, "+").Plain(" comment")
	}
	return c.Colored(ansiDim, "·").Plain(" " + n.Reason)
}

// stateCell builds a Cell for the PR state icon + label. The Render call
// at the use site passes insideDim=!unread so read rows get the dim-safe
// colour closer and unread rows get the full reset.
func stateCell(state notifs.PRState) *Cell {
	icon, label, col := stateGlyph(state)
	return NewCell().Colored(col, icon).Plain(" " + label)
}

// stateGlyph maps a PR state to (icon, label, colour). Falls back to the
// dim "· own" tuple when the state is unknown / not yet fetched.
func stateGlyph(state notifs.PRState) (icon, label, col string) {
	switch state {
	case notifs.PRStateOpen:
		return "●", "open", ansiGreen
	case notifs.PRStateMerged:
		return "●", "merged", ansiPurple
	case notifs.PRStateClosed:
		return "●", "closed", ansiRed
	case notifs.PRStateDraft:
		return "○", "draft", ansiDim
	}
	return "·", "own", ansiDim
}

// prRepoCol is the column index of REPO inside writePRTable's rows.
// Same value with or without BRANCH (BRANCH sits after REPO).
const prRepoCol = 4

// prTitleMax is the upper bound on title length before truncation.
const prTitleMax = 55

func writePRTable(ps []prs.PR, focusedKey string, tty bool, termWidth int, jiraURL string, links bool) {
	// Adaptive fit: try full layout first; if it overflows, drop BRANCH;
	// if it still overflows, also drop the trailing "pass"/"fail"/"wait"
	// status word. The repo column is shrunk last by panelTable.render().
	raw := buildPRRows(ps, focusedKey, tty, true /*branch*/, false /*compact status*/, jiraURL, links)
	if termWidth > 0 && tableWidth(raw) > termWidth {
		raw = buildPRRows(ps, focusedKey, tty, false, false, jiraURL, links)
		if tableWidth(raw) > termWidth {
			raw = buildPRRows(ps, focusedKey, tty, false, true, jiraURL, links)
		}
	}
	t := newPanelTable(raw[0], prRepoCol, termWidth, tty)
	for _, r := range raw[1:] {
		t.addRow(r, false)
	}
	t.render()
}

func buildPRRows(ps []prs.PR, focusedKey string, tty, includeBranch, compactStatus bool, jiraURL string, links bool) [][]string {
	header := []string{"  ", "CHECKS", "REVIEW", "TITLE", "REPO", "#"}
	if includeBranch {
		header = append(header, "BRANCH")
	}
	header = append(header, "AGE")
	if links {
		header = append(header, "LINK")
	}

	rows := make([][]string, 0, len(ps)+1)
	rows = append(rows, dimRow(header, tty))
	for _, p := range ps {
		title := truncate(p.Title, prTitleMax)
		if tty {
			title = styleTickets(title, jiraURL)
		}
		cursor := "  "
		if focusedKey != "" && fmt.Sprintf("%s#%d", p.Repo, p.Number) == focusedKey {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		row := []string{
			cursor,
			prStatusCell(p, compactStatus).Render(tty, false),
			prReviewCell(p).Render(tty, false),
			title,
			p.Repo,
			fmt.Sprintf("#%d", p.Number),
		}
		if includeBranch {
			row = append(row, truncate(p.HeadBranch, 30))
		}
		row = append(row, relativeAge(p.UpdatedAt))
		if links {
			row = append(row, coloredHyperlink(p.URL, "open ↗"))
		}
		rows = append(rows, row)
	}
	return rows
}

func prReviewCell(p prs.PR) *Cell {
	c := NewCell()
	switch p.ReviewDecision {
	case "APPROVED":
		c.Colored(ansiGreen, "✓").Plain(" approved")
	case "CHANGES_REQUESTED":
		c.Colored(ansiRed, "✗").Plain(" changes")
	case "REVIEW_REQUIRED":
		c.Colored(ansiDim, "·").Plain(" blocked")
	default:
		if p.ReviewCount > 0 {
			c.Colored(ansiYellow, "◐").Plain(" reviewed")
		}
		// No decision and no reviewers yet: empty cell (no placeholder).
	}
	if p.CommentCount > 0 {
		if len(c.parts) > 0 {
			c.Plain(" ")
		}
		c.Colored(ansiDim, fmt.Sprintf("+%d", p.CommentCount))
	}
	return c
}

func prStatusCell(p prs.PR, compact bool) *Cell {
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

	c := NewCell()
	switch {
	case p.IsFailing():
		return c.Colored(ansiRed, "✗").Plain(failed())
	case p.IsPending():
		return c.Colored(ansiYellow, "◐").Plain(pending())
	case p.IsPassing():
		return c.Colored(ansiGreen, "✓").Plain(passed())
	case p.Total == 0:
		c.Colored(ansiDim, "·")
		if !compact {
			c.Plain(" none")
		}
		return c
	}
	return c.Colored(ansiDim, "·").Plain(" " + p.State)
}

// runsRepoCol is the column index of REPO inside writeTable's rows.
const runsRepoCol = 3

func writeTable(rs []runs.Run, focusedID string, tty bool, termWidth int, links bool, readIDs map[int64]bool) {
	headers := []string{"  ", "STATUS", "WORKFLOW", "REPO", "BRANCH", "AGE"}
	if links {
		headers = append(headers, "LINK")
	}
	t := newPanelTable(dimRow(headers, tty), runsRepoCol, termWidth, tty)
	today := startOfToday()
	for _, r := range rs {
		dim := (!r.IsActive() && r.UpdatedAt.Before(today)) || readIDs[r.ID]
		cursor := "  "
		if focusedID != "" && strconv.FormatInt(r.ID, 10) == focusedID {
			cursor = color(ansiYellow, "▶", tty) + " "
		}
		row := []string{
			cursor,
			statusCell(r).Render(tty, dim),
			truncate(r.WorkflowName, 30),
			r.Repo,
			truncate(r.Branch, 30),
			ageString(r),
		}
		if links {
			var link string
			if dim {
				link = hyperlink(r.URL, "open ↗")
			} else {
				link = coloredHyperlink(r.URL, "open ↗")
			}
			row = append(row, link)
		}
		t.addRow(row, dim && tty)
	}
	t.render()
}

// TermWidth returns the terminal's column count via term.GetSize. Returns 0
// when stdin/stdout isn't a tty or the size can't be determined (caller
// treats 0 as "unconstrained, don't shrink"). Cross-platform: term.GetSize
// uses an ioctl on Unix and GetConsoleScreenBufferInfo on Windows, so this
// works without shelling out to `stty`.
func TermWidth() int {
	if !isTTY(os.Stdout) || !isTTY(os.Stdin) {
		return 0
	}
	cols, _, err := term.GetSize(int(os.Stdout.Fd()))
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
//   - Completed runs triggered by the viewer are kept for runsWindow (7 days);
//     runs from before today are dimmed by writeTable, older ones drop off.
//   - Anything else drops as soon as it finishes.
//
// Result is sorted by UpdatedAt desc (newest first) then truncated to
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
		case viewerLogin != "" && r.ActorLogin == viewerLogin && time.Since(r.UpdatedAt) < runsWindow:
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
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

func statusCell(r runs.Run) *Cell {
	c := NewCell()
	if r.IsActive() {
		return c.Colored(ansiYellow, "●").Plain(" " + r.Status)
	}
	switch r.Conclusion {
	case "success":
		return c.Colored(ansiGreen, "✓").Plain(" success")
	case "failure", "timed_out", "startup_failure":
		return c.Colored(ansiRed, "✗").Plain(" " + r.Conclusion)
	case "cancelled", "skipped":
		return c.Colored(ansiDim, "·").Plain(" " + r.Conclusion)
	}
	return c.Plain(r.Conclusion)
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

func startOfToday() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
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

// styleTickets wraps every ticket-like substring in amber. When jiraURL is
// non-empty, each match also gets an OSC 8 hyperlink to jiraURL/browse/TICKET.
// Same dim-wrap caveat as coloredHyperlink: callers must not use this for
// cells wrapped in an outer row dim.
func styleTickets(s, jiraURL string) string {
	return ticketRe.ReplaceAllStringFunc(s, func(match string) string {
		if jiraURL != "" {
			ticketID := strings.Trim(match, "[]")
			return ticketHyperlink(jiraURL+"/browse/"+ticketID, match)
		}
		return ansiAmber + match + ansiReset
	})
}

// ticketHyperlink wraps text in an OSC 8 hyperlink with amber styling.
// Same dim-wrap caveat as coloredHyperlink.
func ticketHyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + ansiAmber + text + ansiReset + "\x1b]8;;\x1b\\"
}

// FindTicket returns the first Jira-style ticket ID (brackets stripped) found
// in s, or "" if none. Used by the watch loop to extract a ticket from a
// focused row's title without a direct ticketRe dependency.
func FindTicket(s string) string {
	return strings.Trim(ticketRe.FindString(s), "[]")
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
// being echoed, and hides the cursor. Returns the saved terminal state to pass
// to ExitAltScreen.
func EnterAltScreen() *term.State {
	if !isTTY(os.Stdout) {
		return nil
	}
	saved, _ := term.GetState(int(os.Stdin.Fd()))
	sttyApply("-echo", "-icanon")
	fmt.Fprint(os.Stdout, "\x1b[?1049h\x1b[H\x1b[?25l")
	return saved
}

// ExitAltScreen restores the primary screen buffer, restores the cursor, and
// restores the terminal mode captured by EnterAltScreen. The restore is a
// direct ioctl (not a subprocess) so it completes atomically before the
// process exits.
func ExitAltScreen(saved *term.State) {
	if !isTTY(os.Stdout) {
		return
	}
	fmt.Fprint(os.Stdout, "\x1b[?25h\x1b[?1049l")
	if saved != nil {
		_ = term.Restore(int(os.Stdin.Fd()), saved)
	}
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

// colorInsideDim wraps s in a foreground colour, closing with default-fg
// rather than a full SGR reset. Use inside cells that will sit within an
// outer ansiDim row wrap (read notification rows): the dim survives the
// inner span, so the icon shows up in a muted colour and the rest of the
// row stays uniformly dim.
func colorInsideDim(code, s string, tty bool) string {
	if !tty {
		return s
	}
	return code + s + ansiDefaultFg
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

// SupportsLinks reports whether the current terminal is known to support OSC 8
// hyperlinks. When false, callers should omit the LINK column since the text
// would be inert. Conservative: defaults to false for unknown terminals.
func SupportsLinks() bool {
	if !isTTY(os.Stdout) {
		return false
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "kitty", "ghostty":
		return true
	}
	if os.Getenv("VTE_VERSION") != "" { // GNOME Terminal / Tilix / etc.
		return true
	}
	if os.Getenv("WT_SESSION") != "" { // Windows Terminal
		return true
	}
	if os.Getenv("TERM") == "xterm-kitty" {
		return true
	}
	return false
}
