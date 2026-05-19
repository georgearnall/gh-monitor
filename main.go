package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
	"github.com/georgearnall/gha-monitor/internal/notifs"
	"github.com/georgearnall/gha-monitor/internal/notify"
	"github.com/georgearnall/gha-monitor/internal/prs"
	"github.com/georgearnall/gha-monitor/internal/runs"
	"github.com/georgearnall/gha-monitor/internal/state"
	"github.com/georgearnall/gha-monitor/internal/ui"
	"golang.org/x/sync/errgroup"
)

type stringSet map[string]bool

func (s *stringSet) String() string {
	if s == nil || *s == nil {
		return ""
	}
	keys := make([]string, 0, len(*s))
	for k := range *s {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

func (s *stringSet) Set(v string) error {
	if *s == nil {
		*s = stringSet{}
	}
	(*s)[v] = true
	return nil
}

type watchConfig struct {
	maxRepos    int
	repoRefresh time.Duration

	baseInterval   time.Duration
	activeInterval time.Duration
	lowQuotaFloor  time.Duration
	lowQuotaLimit  int

	prSince  time.Duration // hide PRs not updated within this window (0 disables)
	once     bool
	excluded stringSet
	noNotify bool
	sound    bool
}

type pollResult struct {
	Repos       []discovery.Repo
	Runs        []runs.Run
	PRs         []prs.PR
	Notifs      []notifs.Notification
	ViewerLogin string
	RateLimit   ghclient.RateLimit
	PollErr     error
	PRErr       error
	NotifErr    error
	DiscErr     error
}

func main() {
	fs := flag.NewFlagSet("gha-monitor", flag.ExitOnError)
	cfg := watchConfig{
		baseInterval:   60 * time.Second,
		activeInterval: 20 * time.Second,
		lowQuotaFloor:  2 * time.Minute,
		lowQuotaLimit:  500,
	}
	fs.IntVar(&cfg.maxRepos, "max-repos", 20, "maximum number of repos to monitor")
	fs.DurationVar(&cfg.activeInterval, "interval", cfg.activeInterval, "poll interval while runs are active")
	fs.DurationVar(&cfg.baseInterval, "idle-interval", cfg.baseInterval, "poll interval while no runs are active")
	fs.DurationVar(&cfg.repoRefresh, "repo-refresh", 5*time.Minute, "repo list refresh interval")
	fs.DurationVar(&cfg.prSince, "pr-since", 60*24*time.Hour, "hide pull requests not updated within this duration (0 disables)")
	fs.BoolVar(&cfg.once, "once", false, "run a single poll cycle and exit")
	fs.Var(&cfg.excluded, "exclude", "owner/repo to exclude from monitoring (repeatable)")
	fs.BoolVar(&cfg.noNotify, "no-notify", false, "suppress desktop notifications")
	fs.BoolVar(&cfg.sound, "sound", false, "also play an audible alert on failure")
	fs.Usage = func() { fmt.Fprint(os.Stderr, helpText) }

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !startsWithDash(args[0]) {
		cmd = args[0]
		args = args[1:]
	}
	if cmd == "help" || cmd == "--help" {
		fmt.Print(helpText)
		return
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	client, err := ghclient.New()
	if err != nil {
		fail("auth: %v", err)
	}

	switch cmd {
	case "", "watch":
		runWatch(client, cfg)
	case "list-repos":
		runListRepos(client, cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(2)
	}
}

const helpText = `gh-monitor - watch GitHub Actions runs, PR checks, and notifications

USAGE
  gh-monitor [flags]              Start the live watch TUI (default)
  gh-monitor watch [flags]        Same as above, explicit
  gh-monitor list-repos [flags]   Print the discovered repo set and exit
  gh-monitor help                 Show this help

FLAGS
  --max-repos N         Cap the monitored repo set (default 20)
  --interval D          Poll interval while runs are active (default 20s)
  --idle-interval D     Poll interval when nothing is running (default 1m)
  --repo-refresh D      How often to re-run discovery (default 5m)
  --pr-since D          Hide PRs not updated within this duration (default
                        1440h = 60 days). Use 0 to disable the filter.
  --once                Single poll cycle then exit (no TUI, script-friendly)
  --exclude owner/repo  Skip a noisy repo. Repeatable.
  --no-notify           Suppress desktop notifications on workflow failure
  --sound               Also play a system sound on failure

KEYBINDINGS (watch mode)
  ↑ / ↓                 Move cursor across all three panels (notifications,
                        PRs, and workflow runs)
  ↵  (enter)            Open focused row in browser. If it's a notification,
                        also mark it read.
  m                     Mark focused notification read (no-op on PRs/runs)
  M                     Mark every visible unread notification read
  d                     Dismiss (mark as done) the focused notification.
                        Removes it from the inbox entirely.
  r  /  R  /  space     Refresh now (don't wait for the next interval)
  q  /  Q  /  Ctrl-C    Quit cleanly, restore terminal, save state

PANELS
  NOTIFICATIONS  Inbound mentions, review requests, replies on threads
                 you're in, and activity on PRs you authored or were
                 assigned to. Unread first then most-recent. Read items
                 stay dimmed for 7 days then drop off.
  PULL REQUESTS  Your open non-draft PRs across all of GitHub, with check
                 rollup, review decision, and comment count. Failing
                 first, then pending, then most-recent.
  WORKFLOW RUNS  Up to 10 rows. Active runs always shown (any actor);
                 completed runs only if you triggered them in the last
                 24h. Renovate / Dependabot / Copilot runs are filtered.

DISCOVERY
  The monitored repo set is the union of three queries, deduped and
  sorted by most recent activity:
    1. /user/repos?affiliation=owner,collaborator
    2. /search/issues?q=author:@me+is:pr+is:open
    3. /users/<you>/events  (catches team-membership repos)

STATE
  Persisted to $XDG_CONFIG_HOME/gh-monitor/state.json (or
  ~/.config/gh-monitor/state.json). Holds the last rendered tables,
  an ETag cache for cheap 304 polling, and a run-ID dedup map so
  failure notifications fire once per run across restarts. Safe to
  delete at any time; repopulated on next launch.

RATE LIMITS
  Conditional GETs use If-None-Match; 304 responses do not count
  against the 5000/hr REST budget. Concurrent in-flight requests are
  bounded to 8. The poll interval doubles when remaining budget drops
  below 500. 403 / 429 responses honour Retry-After.

AUTHENTICATION
  Reuses your gh CLI auth (run 'gh auth login' if you haven't). The
  token needs the notifications scope to populate the NOTIFICATIONS
  panel; everything else uses default scopes.
`

func runListRepos(client *ghclient.Client, cfg watchConfig) {
	repos, err := discovery.Discover(client, cfg.maxRepos)
	if err != nil {
		fail("discover: %v", err)
	}
	repos = filterExcluded(repos, cfg.excluded)
	for _, r := range repos {
		fmt.Printf("%-50s %s\n", r.FullName, r.Activity.Format("2006-01-02 15:04"))
	}
}

func runWatch(client *ghclient.Client, cfg watchConfig) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := state.Load()
	if err != nil {
		fail("load state: %v", err)
	}
	client.SetEtags(st.EtagCache)

	if cfg.once {
		runOnce(ctx, client, cfg, st)
		return
	}

	savedTerm := ui.EnterAltScreen()
	defer ui.ExitAltScreen(savedTerm)
	defer ui.ClearWindowTitle()
	defer func() {
		if err := st.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "save state on exit: %v\n", err)
		}
	}()

	focused := pickFocus(st, focusTarget{})

	// First paint: render whatever's in the cache. <100ms because no network.
	renderFromState(st, cfg, true /*refreshing*/, focused)

	trigger := make(chan struct{}, 1)
	started := make(chan struct{}, 1)
	results := make(chan pollResult, 1)
	keys := make(chan rune, 8)

	go readKeys(ctx, keys)
	go producerLoop(ctx, client, &cfg, trigger, started, results)

	enqueue(trigger) // initial refresh

	winchRaw, stopWinch := notifyWinch()
	defer stopWinch()
	winch := coalesceSignal(ctx, winchRaw, 100*time.Millisecond)

	var (
		refreshing bool
		nextTimer  *time.Timer
	)

	for {
		select {
		case <-started:
			refreshing = true
			renderFromState(st, cfg, refreshing, focused)

		case <-winch:
			// On resize the alternate screen often retains wrapped
			// fragments of the previous render that the in-place
			// overwrite path won't touch. One-off full clear here.
			fmt.Print("\x1b[H\x1b[2J")
			renderFromState(st, cfg, refreshing, focused)

		case res := <-results:
			refreshing = false
			applyResult(st, &cfg, res)
			st.EtagCache = client.Etags()
			focused = pickFocus(st, focused)
			renderFromState(st, cfg, false, focused)
			if err := st.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "save state: %v\n", err)
			}
			next := cfg.nextInterval(activeCount(res.Runs), res.RateLimit, res.PollErr)
			if nextTimer != nil {
				nextTimer.Stop()
			}
			nextTimer = time.AfterFunc(next, func() { enqueue(trigger) })

		case k := <-keys:
			switch k {
			case 'r', 'R', ' ':
				if nextTimer != nil {
					nextTimer.Stop()
				}
				enqueue(trigger)
			case keyDown:
				focused = moveFocus(st, focused, +1)
				renderFromState(st, cfg, refreshing, focused)
			case keyUp:
				focused = moveFocus(st, focused, -1)
				renderFromState(st, cfg, refreshing, focused)
			case '\r', '\n':
				openFocused(st, focused)
				markFocusedRead(client, st, &cfg, focused)
			case 'm':
				markFocusedRead(client, st, &cfg, focused)
			case 'M':
				markAllVisibleRead(client, st, &cfg, focused)
			case 'd':
				focused = dismissFocused(client, st, &cfg, focused)
			case 'q', 'Q':
				return
			}

		case <-ctx.Done():
			return
		}
	}
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

// dismissFocused removes the focused notification both locally and on
// GitHub via DELETE /notifications/threads/{id}. No-op if the focused
// row is not in the notifications panel. Fires the DELETE in the
// background after an optimistic local removal so the UI feels instant.
func dismissFocused(client *ghclient.Client, st *state.State, cfg *watchConfig, f focusTarget) focusTarget {
	if f.Panel != "notifs" || f.ID == "" {
		return f
	}
	newFocus, ok := applyDismiss(st, f.ID)
	if !ok {
		return f
	}
	dismissedID := f.ID
	renderFromState(st, *cfg, false, newFocus)
	go func() {
		if err := notifs.DismissAll(client, []string{dismissedID}); err != nil {
			fmt.Fprintf(os.Stderr, "dismiss: %v\n", err)
		}
	}()
	return newFocus
}

// markFocusedRead marks the focused notification read. No-op if the focused
// row is not in the notifications panel or is already read.
func markFocusedRead(client *ghclient.Client, st *state.State, cfg *watchConfig, f focusTarget) {
	if f.Panel != "notifs" || f.ID == "" {
		return
	}
	var matched string
	for i := range st.LastNotifs {
		if st.LastNotifs[i].ID == f.ID && st.LastNotifs[i].Unread {
			st.LastNotifs[i].Unread = false
			matched = f.ID
			break
		}
	}
	if matched == "" {
		return
	}
	renderFromState(st, *cfg, false, f)
	go func() {
		if err := notifs.MarkAllRead(client, []string{matched}); err != nil {
			fmt.Fprintf(os.Stderr, "mark read: %v\n", err)
		}
	}()
}

// markAllVisibleRead optimistically flips every unread notification in the
// cached snapshot to read, repaints, then fires PATCH calls in background.
func markAllVisibleRead(client *ghclient.Client, st *state.State, cfg *watchConfig, f focusTarget) {
	ids := make([]string, 0, len(st.LastNotifs))
	for i := range st.LastNotifs {
		if st.LastNotifs[i].Unread {
			ids = append(ids, st.LastNotifs[i].ID)
			st.LastNotifs[i].Unread = false
		}
	}
	if len(ids) == 0 {
		return
	}
	renderFromState(st, *cfg, false, f)
	go func() {
		if err := notifs.MarkAllRead(client, ids); err != nil {
			fmt.Fprintf(os.Stderr, "mark read: %v\n", err)
		}
	}()
}

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

// coalesceSignal forwards events from in to the returned channel, collapsing
// a burst of signals into a single event delivered once the burst has been
// quiet for d. Used to debounce SIGWINCH during a resize drag, which can
// fire 30+ times a second and produce visible flicker if each one triggers
// a full screen redraw.
//
// Cancels cleanly when ctx is done.
func coalesceSignal(ctx context.Context, in <-chan os.Signal, d time.Duration) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		var timer *time.Timer
		fire := func() {
			select {
			case out <- struct{}{}:
			default:
			}
		}
		for {
			select {
			case <-in:
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(d, fire)
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			}
		}
	}()
	return out
}

// enqueue sends to a single-slot channel without blocking when full.
func enqueue(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// producerLoop owns the refresh pipeline. One refresh runs at a time: each
// `trigger` consumed yields one `started` signal, then one `pollResult`.
// Coalescing happens at the trigger channel (single-slot buffer).
func producerLoop(
	ctx context.Context,
	client *ghclient.Client,
	cfg *watchConfig,
	trigger <-chan struct{},
	started chan<- struct{},
	results chan<- pollResult,
) {
	for {
		select {
		case <-trigger:
		case <-ctx.Done():
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		res := doRefresh(ctx, client, cfg)
		select {
		case results <- res:
		case <-ctx.Done():
			return
		}
	}
}

// Synthetic rune values for non-character keys. Chosen from the Unicode
// private-use area so they can flow through a chan rune alongside ASCII.
const (
	keyUp    rune = 0xE001
	keyDown  rune = 0xE002
	keyRight rune = 0xE003
	keyLeft  rune = 0xE004
)

// readKeys forwards stdin keystrokes to the keys channel. Plain ASCII bytes
// are forwarded as-is. CSI escape sequences (ESC [ A/B/C/D) are parsed into
// the synthetic keyUp/Down/Right/Left runes so the consumer can switch on
// them like any other key. Unknown CSI sequences are dropped silently.
func readKeys(ctx context.Context, keys chan<- rune) {
	r := bufio.NewReader(os.Stdin)
	forward := func(k rune) bool {
		select {
		case keys <- k:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		c, err := r.ReadByte()
		if err != nil {
			return
		}
		if c != 0x1B {
			if !forward(rune(c)) {
				return
			}
			continue
		}
		// ESC: try to consume a CSI sequence.
		next, err := r.ReadByte()
		if err != nil {
			return
		}
		if next != '[' {
			// Lone ESC or an unrelated escape; forward the second byte as a
			// regular keypress so the user's follow-up still registers.
			if !forward(rune(next)) {
				return
			}
			continue
		}
		third, err := r.ReadByte()
		if err != nil {
			return
		}
		var arrow rune
		switch third {
		case 'A':
			arrow = keyUp
		case 'B':
			arrow = keyDown
		case 'C':
			arrow = keyRight
		case 'D':
			arrow = keyLeft
		default:
			continue
		}
		if !forward(arrow) {
			return
		}
	}
}

func runOnce(ctx context.Context, client *ghclient.Client, cfg watchConfig, st *state.State) {
	res := doRefresh(ctx, client, &cfg)
	if res.DiscErr != nil {
		fmt.Fprintf(os.Stderr, "discover: %v\n", res.DiscErr)
	}
	if res.PollErr != nil {
		fmt.Fprintf(os.Stderr, "poll: %v\n", res.PollErr)
	}
	if res.PRErr != nil {
		fmt.Fprintf(os.Stderr, "pr poll: %v\n", res.PRErr)
	}
	if res.NotifErr != nil {
		fmt.Fprintf(os.Stderr, "notif poll: %v\n", res.NotifErr)
	}
	applyResult(st, &cfg, res)
	st.EtagCache = client.Etags()
	renderFromState(st, cfg, false, focusTarget{})
	if err := st.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "save state: %v\n", err)
	}
}

// doRefresh runs one synchronous discover + poll pass. Workflow-run polling
// and PR check polling fan out concurrently.
func doRefresh(ctx context.Context, client *ghclient.Client, cfg *watchConfig) pollResult {
	res := pollResult{}
	repos, discErr := discovery.Discover(client, cfg.maxRepos)
	if discErr != nil {
		res.DiscErr = discErr
		// Fall through. PR and notification polls are independent of the
		// repo list, so a discovery hiccup should not blank those panels.
	}
	repos = filterExcluded(repos, cfg.excluded)
	res.Repos = repos

	g, _ := errgroup.WithContext(ctx)
	g.Go(func() error {
		if discErr != nil {
			return nil
		}
		polled, pollErr := runs.Poll(client, repos)
		res.Runs = polled
		res.PollErr = pollErr
		return nil
	})
	g.Go(func() error {
		var since time.Time
		if cfg.prSince > 0 {
			since = time.Now().Add(-cfg.prSince)
		}
		pulled, prErr := prs.Poll(client, since)
		res.PRs = pulled
		res.PRErr = prErr
		return nil
	})
	g.Go(func() error {
		ns, notifErr := notifs.Poll(client)
		res.Notifs = ns
		res.NotifErr = notifErr
		return nil
	})
	_ = g.Wait()

	if login, err := client.Viewer(); err == nil {
		res.ViewerLogin = login
	}
	res.RateLimit = client.RateLimit()
	return res
}

// applyResult updates state.Runs, fires notifications, and updates the cache
// snapshot fields used for fast startup next time.
func applyResult(st *state.State, cfg *watchConfig, res pollResult) {
	if res.DiscErr != nil || res.PollErr != nil || res.PRErr != nil || res.NotifErr != nil {
		// Still update RateLimit so the footer reflects reality.
		st.LastRateLimit = res.RateLimit
	}
	if res.Runs == nil && res.Repos == nil && res.PRs == nil && res.Notifs == nil {
		return
	}
	seen := make(map[int64]bool, len(res.Runs))
	for _, r := range res.Runs {
		seen[r.ID] = true
		if st.Observe(r) == state.TransitionFailure {
			if !cfg.noNotify {
				if err := notify.Failure(r.Repo, r.WorkflowName, r.Branch, r.URL); err != nil {
					fmt.Fprintf(os.Stderr, "notify: %v\n", err)
				}
			}
			if cfg.sound {
				notify.PlayAlert()
			}
		}
	}
	st.Prune(seen, 7*24*time.Hour)
	// Only overwrite each cached panel when its source poll actually succeeded.
	// A transient failure in one branch should leave the other panels alone
	// (and keep the stale data visible) rather than blanking everything.
	if res.DiscErr == nil && res.PollErr == nil {
		st.LastView = res.Runs
	}
	if res.PRErr == nil && res.PRs != nil {
		st.LastPRs = res.PRs
	}
	if res.NotifErr == nil && res.Notifs != nil {
		st.LastNotifs = res.Notifs
	}
	if res.DiscErr == nil && res.Repos != nil {
		st.Repos = res.Repos
	}
	st.LastPoll = time.Now()
	st.LastRateLimit = res.RateLimit
	if res.ViewerLogin != "" {
		st.ViewerLogin = res.ViewerLogin
	}
}

func renderFromState(st *state.State, cfg watchConfig, refreshing bool, f focusTarget) {
	stale := refreshing && !st.LastPoll.IsZero()
	var next time.Duration
	if !refreshing {
		next = cfg.nextInterval(activeCount(st.LastView), st.LastRateLimit, nil)
	}
	snap := ui.Snapshot{
		Runs:          st.LastView,
		PRs:           st.LastPRs,
		Notifs:        st.LastNotifs,
		ViewerLogin:   st.ViewerLogin,
		RepoCount:     len(st.Repos),
		RateRemaining: st.LastRateLimit.Remaining,
		RateLimit:     st.LastRateLimit.Limit,
		PolledAt:      st.LastPoll,
		NextPollIn:    next,
		TermWidth:     ui.TermWidth(),
		Stale:         stale,
		Refreshing:    refreshing,
	}
	switch f.Panel {
	case "notifs":
		snap.FocusedNotifID = f.ID
	case "prs":
		snap.FocusedPRKey = f.ID
	case "runs":
		snap.FocusedRunID = f.ID
	}
	ui.Render(snap)
}

func activeCount(rs []runs.Run) int {
	n := 0
	for _, r := range rs {
		if r.IsActive() {
			n++
		}
	}
	return n
}

func (c watchConfig) nextInterval(active int, rl ghclient.RateLimit, pollErr error) time.Duration {
	if rlErr, ok := ghclient.AsRateLimited(pollErr); ok {
		return max(rlErr.RetryAfter, c.lowQuotaFloor)
	}
	d := c.baseInterval
	if active > 0 {
		d = c.activeInterval
	}
	if rl.Limit > 0 && rl.Remaining < c.lowQuotaLimit {
		if c.lowQuotaFloor > d {
			d = c.lowQuotaFloor
		}
	}
	return d
}

func filterExcluded(repos []discovery.Repo, excl stringSet) []discovery.Repo {
	if len(excl) == 0 {
		return repos
	}
	out := repos[:0]
	for _, r := range repos {
		if excl[r.FullName] {
			continue
		}
		out = append(out, r)
	}
	return out
}


func startsWithDash(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
