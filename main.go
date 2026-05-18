package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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
	fs.BoolVar(&cfg.once, "once", false, "run a single poll cycle and exit")
	fs.Var(&cfg.excluded, "exclude", "owner/repo to exclude from monitoring (repeatable)")
	fs.BoolVar(&cfg.noNotify, "no-notify", false, "suppress desktop notifications")
	fs.BoolVar(&cfg.sound, "sound", false, "also play an audible alert on failure")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gha-monitor [flags] [list-repos|watch]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !startsWithDash(args[0]) {
		cmd = args[0]
		args = args[1:]
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
		fmt.Fprintf(os.Stderr, "unknown command: %q\n", cmd)
		os.Exit(2)
	}
}

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

	focusedID := pickFocus(st.LastNotifs, "")

	// First paint: render whatever's in the cache. <100ms because no network.
	renderFromState(st, cfg, true /*refreshing*/, focusedID)

	trigger := make(chan struct{}, 1)
	started := make(chan struct{}, 1)
	results := make(chan pollResult, 1)
	keys := make(chan rune, 8)

	go readKeys(ctx, keys)
	go producerLoop(ctx, client, &cfg, trigger, started, results)

	enqueue(trigger) // initial refresh

	spinnerTick := time.NewTicker(120 * time.Millisecond)
	defer spinnerTick.Stop()

	var (
		refreshing   bool
		spinnerFrame int
		nextTimer    *time.Timer
	)

	for {
		select {
		case <-started:
			refreshing = true
			spinnerFrame = 0
			ui.RenderSpinner(spinnerFrame)

		case res := <-results:
			refreshing = false
			applyResult(st, &cfg, res)
			st.EtagCache = client.Etags()
			focusedID = pickFocus(st.LastNotifs, focusedID)
			renderFromState(st, cfg, false, focusedID)
			if err := st.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "save state: %v\n", err)
			}
			next := cfg.nextInterval(activeCount(res.Runs), res.RateLimit, res.PollErr)
			if nextTimer != nil {
				nextTimer.Stop()
			}
			nextTimer = time.AfterFunc(next, func() { enqueue(trigger) })

		case <-spinnerTick.C:
			if refreshing {
				spinnerFrame++
				ui.RenderSpinner(spinnerFrame)
			}

		case k := <-keys:
			switch k {
			case 'r', 'R', ' ':
				if nextTimer != nil {
					nextTimer.Stop()
				}
				enqueue(trigger)
			case keyDown:
				focusedID = moveFocus(st.LastNotifs, focusedID, +1)
				renderFromState(st, cfg, refreshing, focusedID)
			case keyUp:
				focusedID = moveFocus(st.LastNotifs, focusedID, -1)
				renderFromState(st, cfg, refreshing, focusedID)
			case '\r', '\n':
				openFocused(st, focusedID)
			case 'm':
				markFocusedRead(client, st, &cfg, focusedID)
			case 'M':
				markAllVisibleRead(client, st, &cfg, focusedID)
			case 'q', 'Q':
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

// openFocused launches the URL of the focused notification in the user's
// default browser. Non-blocking: doesn't wait for the browser to exit.
func openFocused(st *state.State, focusedID string) {
	if focusedID == "" {
		return
	}
	var url string
	for _, n := range st.LastNotifs {
		if n.ID == focusedID {
			url = n.URL
			break
		}
	}
	if url == "" {
		return
	}
	if err := exec.Command("open", url).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
	}
}

// markFocusedRead marks the single focused notification read. No-op if the
// focused row is already read or no row is focused.
func markFocusedRead(client *ghclient.Client, st *state.State, cfg *watchConfig, focusedID string) {
	if focusedID == "" {
		return
	}
	var matched string
	for i := range st.LastNotifs {
		if st.LastNotifs[i].ID == focusedID && st.LastNotifs[i].Unread {
			st.LastNotifs[i].Unread = false
			matched = focusedID
			break
		}
	}
	if matched == "" {
		return
	}
	renderFromState(st, *cfg, false, focusedID)
	go func() {
		if err := notifs.MarkAllRead(client, []string{matched}); err != nil {
			fmt.Fprintf(os.Stderr, "mark read: %v\n", err)
		}
	}()
}

// markAllVisibleRead optimistically flips every unread notification in the
// cached snapshot to read, repaints immediately so the UI dims them, then
// fires PATCH calls in the background.
func markAllVisibleRead(client *ghclient.Client, st *state.State, cfg *watchConfig, focusedID string) {
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
	renderFromState(st, *cfg, false, focusedID)
	go func() {
		if err := notifs.MarkAllRead(client, ids); err != nil {
			fmt.Fprintf(os.Stderr, "mark read: %v\n", err)
		}
	}()
}

// pickFocus returns currentID if it still appears in ns, otherwise falls
// back to the first unread notification, otherwise the first overall.
// Returns "" only when ns is empty.
func pickFocus(ns []notifs.Notification, currentID string) string {
	if currentID != "" {
		for _, n := range ns {
			if n.ID == currentID {
				return currentID
			}
		}
	}
	for _, n := range ns {
		if n.Unread {
			return n.ID
		}
	}
	if len(ns) > 0 {
		return ns[0].ID
	}
	return ""
}

// moveFocus advances focus by delta rows (positive = down, negative = up),
// clamped to the bounds of ns. Returns "" if ns is empty.
func moveFocus(ns []notifs.Notification, currentID string, delta int) string {
	if len(ns) == 0 {
		return ""
	}
	idx := -1
	for i, n := range ns {
		if n.ID == currentID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ns[0].ID
	}
	next := idx + delta
	if next < 0 {
		next = 0
	}
	if next >= len(ns) {
		next = len(ns) - 1
	}
	return ns[next].ID
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
	renderFromState(st, cfg, false, "")
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
		pulled, prErr := prs.Poll(client)
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

func renderFromState(st *state.State, cfg watchConfig, refreshing bool, focusedID string) {
	stale := refreshing && !st.LastPoll.IsZero()
	var next time.Duration
	if !refreshing {
		next = cfg.nextInterval(activeCount(st.LastView), st.LastRateLimit, nil)
	}
	ui.Render(ui.Snapshot{
		Runs:           st.LastView,
		PRs:            st.LastPRs,
		Notifs:         st.LastNotifs,
		FocusedNotifID: focusedID,
		ViewerLogin:    st.ViewerLogin,
		RepoCount:      len(st.Repos),
		RateRemaining:  st.LastRateLimit.Remaining,
		RateLimit:      st.LastRateLimit.Limit,
		PolledAt:       st.LastPoll,
		NextPollIn:     next,
		Stale:          stale,
		Refreshing:     refreshing,
	})
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
