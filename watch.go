package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/georgearnall/gh-monitor/internal/discovery"
	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/notify"
	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
	"github.com/georgearnall/gh-monitor/internal/state"
	"github.com/georgearnall/gh-monitor/internal/ui"
	"golang.org/x/sync/errgroup"
)

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
	jiraURL  string // session override; does not write to state

	// Discovery cache: populated by doRefresh, reused until repoRefresh elapses.
	lastDiscovery time.Time
	cachedRepos   []discovery.Repo
}

// notifyFailure/notifyComment/notifyNewNotification are indirections over
// the notify package's OS-shelling alert functions, swappable in tests so
// applyResult's alert-gating logic can be asserted without actually sending
// a desktop notification.
var (
	notifyFailure         = notify.Failure
	notifyComment         = notify.Comment
	notifyNewNotification = notify.NewNotification
)

type pollResult struct {
	Repos       []discovery.Repo
	Runs        []runs.Run
	PRs         []prs.PR
	AssignedPRs []prs.PR
	Notifs      []notifs.Notification
	ViewerLogin string
	RateLimit   ghclient.RateLimit
	PollErr     error
	PRErr       error
	NotifErr    error
	DiscErr     error
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
	renderFromState(st, cfg, true /*refreshing*/, focused, "")

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

	dismissQueue := make(chan dismissReq, 128)
	dismissOutcomes := make(chan dismissOutcome, 16)
	go dismissWorker(ctx, client, dismissQueue, dismissOutcomes)

	var (
		refreshing bool
		nextTimer  *time.Timer
		configMode bool
		ps         promptState
	)
	render := func() {
		if configMode {
			renderConfig(st, cfg)
		} else {
			var pl string
			if ps.active {
				pl = "Jira base URL: " + ps.buffer + "▌"
			}
			renderFromState(st, cfg, refreshing, focused, pl)
		}
	}

	for {
		select {
		case <-started:
			refreshing = true
			render()

		case <-winch:
			// On resize the alternate screen often retains wrapped
			// fragments of the previous render that the in-place
			// overwrite path won't touch. One-off full clear here.
			fmt.Print("\x1b[H\x1b[2J")
			render()

		case res := <-results:
			refreshing = false
			applyResult(st, &cfg, res)
			st.EtagCache = client.Etags()
			focused = pickFocus(st, focused)
			render()
			if err := st.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "save state: %v\n", err)
			}
			next := cfg.nextInterval(activeCount(st.LastView), res.RateLimit, res.PollErr)
			if nextTimer != nil {
				nextTimer.Stop()
			}
			nextTimer = time.AfterFunc(next, func() { enqueue(trigger) })

		case k := <-keys:
			if ps.active {
				switch k {
				case '\r', '\n':
					input := strings.TrimSpace(ps.buffer)
					cb := ps.onConfirm
					ps = promptState{}
					if cb != nil && input != "" {
						cb(input)
					}
					render()
				case 0x7F, 0x08: // backspace / ctrl-H
					if runes := []rune(ps.buffer); len(runes) > 0 {
						ps.buffer = string(runes[:len(runes)-1])
					}
					render()
				case 0x1B: // escape
					ps = promptState{}
					render()
				default:
					if k >= 0x20 && k < 0x7F {
						ps.buffer += string(k)
						render()
					}
				}
				continue
			}
			if configMode && k != '?' && k != 'q' && k != 'Q' && k != '1' && k != '2' && k != '3' {
				continue
			}
			switch k {
			case 'r', 'R', ' ':
				if nextTimer != nil {
					nextTimer.Stop()
				}
				enqueue(trigger)
			case keyDown:
				focused = moveFocus(st, focused, +1)
				render()
			case keyUp:
				focused = moveFocus(st, focused, -1)
				render()
			case '\r', '\n':
				openFocused(st, focused)
				markFocusedRead(client, st, &cfg, focused)
			case 'm':
				markFocusedRead(client, st, &cfg, focused)
				markFocusedRunRead(st, &cfg, focused)
			case 'd':
				if focused.Panel == "runs" {
					focused = dismissFocusedRun(st, &cfg, focused)
				} else {
					focused = dismissFocused(dismissQueue, st, &cfg, focused)
				}
			case 'x':
				if focused.Panel == "runs" {
					muteFocusedRunRepo(st, &cfg, focused)
				}
			case 't':
				if focused.Panel != "runs" && focused.ID != "" {
					ticket := findTicketInFocused(st, focused)
					if ticket != "" {
						jiraURL := effectiveJiraURLFor(cfg, st)
						if jiraURL != "" {
							openURL(jiraURL + "/browse/" + ticket)
						} else {
							ps = promptState{
								active: true,
								onConfirm: func(input string) {
									input = strings.TrimRight(strings.TrimSpace(input), "/")
									st.JiraURL = input
									if err := st.Save(); err != nil {
										fmt.Fprintf(os.Stderr, "save state: %v\n", err)
									}
									openURL(input + "/browse/" + ticket)
								},
							}
							render()
						}
					}
				}
			case '?':
				configMode = !configMode
				render()
			case '1':
				if configMode {
					st.SetFailedBuildAlerts(!st.FailedBuildAlertsEnabled())
					if err := st.Save(); err != nil {
						fmt.Fprintf(os.Stderr, "save state: %v\n", err)
					}
					render()
				}
			case '2':
				if configMode {
					st.NotifyAllGitHub = !st.NotifyAllGitHub
					if err := st.Save(); err != nil {
						fmt.Fprintf(os.Stderr, "save state: %v\n", err)
					}
					render()
				}
			case '3':
				if configMode {
					st.NotifyOwnPRComments = !st.NotifyOwnPRComments
					if err := st.Save(); err != nil {
						fmt.Fprintf(os.Stderr, "save state: %v\n", err)
					}
					render()
				}
			case 'q', 'Q':
				return
			}

		case out := <-dismissOutcomes:
			if out.Err != nil {
				// Un-record so the notification reappears honestly on
				// the next poll instead of being hidden by the
				// bounce-back cache for 10 minutes.
				delete(st.DismissedNotifs, out.ID)
				st.BgErr = fmt.Sprintf("dismiss failed: %v", out.Err)
				st.BgErrAt = time.Now()
				render()
			}

		case <-ctx.Done():
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
	renderFromState(st, cfg, false, focusTarget{}, "")
	if err := st.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "save state: %v\n", err)
	}
}

// doRefresh runs one synchronous discover + poll pass. Workflow-run polling
// and PR check polling fan out concurrently.
func doRefresh(ctx context.Context, client *ghclient.Client, cfg *watchConfig) pollResult {
	res := pollResult{}

	var (
		repos   []discovery.Repo
		discErr error
	)
	cacheValid := cfg.repoRefresh > 0 &&
		!cfg.lastDiscovery.IsZero() &&
		time.Since(cfg.lastDiscovery) < cfg.repoRefresh
	if cacheValid {
		repos = cfg.cachedRepos
	} else {
		repos, discErr = discovery.Discover(client, cfg.maxRepos)
		if discErr != nil {
			res.DiscErr = discErr
			// Fall through. PR and notification polls are independent of the
			// repo list, so a discovery hiccup should not blank those panels.
		} else {
			cfg.lastDiscovery = time.Now()
			cfg.cachedRepos = repos
		}
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
		authored, assigned, prErr := prs.Poll(client, since)
		res.PRs = authored
		res.AssignedPRs = assigned
		res.PRErr = prErr
		return nil
	})
	g.Go(func() error {
		ns, notifErr := notifs.Poll(client)
		if notifErr == nil && len(ns) > 0 {
			// Enrich with PR state in one extra GraphQL request. Best-
			// effort: any error here just leaves PRState empty and the
			// UI falls back to "· own".
			if states, err := notifs.FetchPRStates(client, ns); err == nil {
				for i := range ns {
					if s, ok := states[ns[i].ID]; ok {
						ns[i].PRState = s
					}
				}
			}
		}
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
		if st.Observe(r) == state.TransitionFailure && !st.IsRunDismissed(r.ID) {
			alerted := false
			if st.FailedBuildAlertsEnabled() && !cfg.noNotify {
				if err := notifyFailure(r.Repo, r.WorkflowName, r.Branch, r.URL); err != nil {
					fmt.Fprintf(os.Stderr, "notify: %v\n", err)
				}
				alerted = true
			}
			if cfg.sound && alerted {
				notify.PlayAlert()
			}
		}
	}
	st.Prune(seen, 7*24*time.Hour)
	// Only overwrite each cached panel when its source poll actually succeeded.
	// A transient failure in one branch should leave the other panels alone
	// (and keep the stale data visible) rather than blanking everything.
	if res.DiscErr == nil && res.PollErr == nil {
		st.PruneRunDismissals(seen, 60*time.Second)
		st.PruneReadRuns(seen, 60*time.Second)
		viewer := res.ViewerLogin
		if viewer == "" {
			viewer = st.ViewerLogin
		}
		filtered := make([]runs.Run, 0, len(res.Runs))
		for _, r := range res.Runs {
			if st.IsRunDismissed(r.ID) {
				continue
			}
			if st.MutedRepos[r.Repo] && r.ActorLogin != viewer {
				continue
			}
			filtered = append(filtered, r)
		}
		st.LastView = filtered
	}
	if res.PRErr == nil && res.PRs != nil {
		st.LastPRs = res.PRs
		st.LastAssignedPRs = res.AssignedPRs
	}
	if res.NotifErr == nil && res.Notifs != nil {
		// Self-healing dismiss prune: drop guards whose ID is no longer
		// in GitHub's response. GitHub keeps echoing done threads
		// indefinitely via ?all=true (community bug #152852), so any
		// fixed time window is wrong; "until GitHub stops echoing it"
		// is the only reliable signal. The 60s floor protects entries
		// just added from a poll that happened before GitHub started
		// echoing them.
		present := make(map[string]bool, len(res.Notifs))
		for _, n := range res.Notifs {
			present[n.ID] = true
		}
		st.PruneDismissedAbsent(present, 60*time.Second)
		st.PruneAlertedNotifsAbsent(present, 60*time.Second)

		// True only for the very first time this notification type has
		// ever been observed (empty ledger): the one situation where we
		// can't tell a thread that's brand new right now apart from one
		// that's been sitting unread since before gh-monitor ever ran.
		// See ObserveNotifAlert.
		coldStart := len(st.AlertedNotifs) == 0

		// PRs authored by the viewer, used below to tell "a comment on my
		// own PR" apart from every other kind of notification. Fall back
		// to the cached set on a transient PR-poll error rather than
		// treating every PR as not-mine.
		authored := res.PRs
		if res.PRErr != nil {
			authored = st.LastPRs
		}
		authoredSet := make(map[string]bool, len(authored))
		for _, p := range authored {
			authoredSet[prKey(p)] = true
		}

		filtered := make([]notifs.Notification, 0, len(res.Notifs))
		for _, n := range res.Notifs {
			if st.IsDismissed(n.ID, n.UpdatedAt) {
				continue
			}
			filtered = append(filtered, n)

			// Bookkeeping always runs regardless of toggle state, so the
			// ledger stays warm — flipping a toggle on shouldn't storm-
			// alert every pre-existing item it never got to observe
			// while off.
			isNew := st.ObserveNotifAlert(n.ID, n.UpdatedAt, coldStart)
			if !isNew || cfg.noNotify {
				continue
			}
			isOwnComment := n.HasCommentAnchor && authoredSet[fmt.Sprintf("%s#%d", n.Repo, n.PRNumber)]
			switch {
			case st.NotifyOwnPRComments && isOwnComment:
				if err := notifyComment(n.Repo, n.PRNumber, n.Title, n.URL); err != nil {
					fmt.Fprintf(os.Stderr, "notify: %v\n", err)
				}
				if cfg.sound {
					notify.PlayAlert()
				}
			case st.NotifyAllGitHub:
				// Reaches here even when isOwnComment is true but
				// NotifyOwnPRComments is off — it's still a new item
				// the user asked to hear about generically. Only one of
				// the two cases ever fires per notification per poll,
				// so an event that qualifies for both never double-alerts.
				if err := notifyNewNotification(n.Repo, n.PRNumber, n.Reason, n.Title, n.URL); err != nil {
					fmt.Fprintf(os.Stderr, "notify: %v\n", err)
				}
				if cfg.sound {
					notify.PlayAlert()
				}
			}
		}
		st.LastNotifs = filtered
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

func renderFromState(st *state.State, cfg watchConfig, refreshing bool, f focusTarget, promptLine string) {
	stale := refreshing && !st.LastPoll.IsZero()
	var next time.Duration
	if !refreshing {
		next = cfg.nextInterval(activeCount(st.LastView), st.LastRateLimit, nil)
	}
	// Surface a recent background error in the footer for ~30s.
	var bgErr string
	if !st.BgErrAt.IsZero() && time.Since(st.BgErrAt) < 30*time.Second {
		bgErr = st.BgErr
	}
	snap := ui.Snapshot{
		Runs:          st.LastView,
		PRs:           st.LastPRs,
		AssignedPRs:   st.LastAssignedPRs,
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
		BgErr:         bgErr,
		JiraURL:       effectiveJiraURLFor(cfg, st),
		PromptLine:    promptLine,
		Links:         ui.SupportsLinks(),
		ReadRunIDs:    readRunIDs(st),
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

func readRunIDs(st *state.State) map[int64]bool {
	if len(st.MarkedReadRuns) == 0 {
		return nil
	}
	out := make(map[int64]bool, len(st.MarkedReadRuns))
	for id := range st.MarkedReadRuns {
		out[id] = true
	}
	return out
}

func effectiveJiraURLFor(cfg watchConfig, st *state.State) string {
	u := cfg.jiraURL
	if u == "" {
		u = st.JiraURL
	}
	return strings.TrimRight(strings.TrimSpace(u), "/")
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

// promptState holds the state for the inline URL-input prompt. When active,
// all keypresses are routed to the buffer; Enter calls onConfirm; Esc cancels.
type promptState struct {
	active    bool
	buffer    string
	onConfirm func(string)
}

// findTicketInFocused returns the first Jira ticket ID found in the focused
// row's title (brackets stripped), or "" if none found or panel has no title.
func findTicketInFocused(st *state.State, f focusTarget) string {
	var title string
	switch f.Panel {
	case "notifs":
		for _, n := range st.LastNotifs {
			if n.ID == f.ID {
				title = n.Title
				break
			}
		}
	case "prs":
		for _, p := range st.LastPRs {
			if prKey(p) == f.ID {
				title = p.Title
				break
			}
		}
		if title == "" {
			for _, p := range st.LastAssignedPRs {
				if prKey(p) == f.ID {
					title = p.Title
					break
				}
			}
		}
	}
	return ui.FindTicket(title)
}

func renderConfig(st *state.State, cfg watchConfig) {
	excl := make([]string, 0, len(cfg.excluded))
	for r := range cfg.excluded {
		excl = append(excl, r)
	}
	sort.Strings(excl)
	repos := make([]ui.RepoStatus, 0, len(st.Repos))
	for _, r := range st.Repos {
		repos = append(repos, ui.RepoStatus{
			Name:     r.FullName,
			Activity: r.Activity,
			Muted:    st.MutedRepos[r.FullName],
		})
	}
	sort.Slice(repos, func(i, j int) bool {
		return repos[i].Activity.After(repos[j].Activity)
	})
	ui.RenderConfig(ui.ConfigSnapshot{
		Repos:          repos,
		ExcludedRepos:  excl,
		ViewerLogin:    st.ViewerLogin,
		BaseInterval:   cfg.baseInterval,
		ActiveInterval: cfg.activeInterval,
		RepoRefresh:    cfg.repoRefresh,
		MaxRepos:       cfg.maxRepos,
		PRSince:        cfg.prSince,
		RateRemaining:  st.LastRateLimit.Remaining,
		RateLimit:      st.LastRateLimit.Limit,
		JiraURL:        effectiveJiraURLFor(cfg, st),
		TermWidth:      ui.TermWidth(),

		NotifyFailedBuilds:  st.FailedBuildAlertsEnabled(),
		NotifyAllGitHub:     st.NotifyAllGitHub,
		NotifyOwnPRComments: st.NotifyOwnPRComments,
	})
}
