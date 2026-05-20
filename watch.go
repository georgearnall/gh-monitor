package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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

	// Discovery cache: populated by doRefresh, reused until repoRefresh elapses.
	lastDiscovery time.Time
	cachedRepos   []discovery.Repo
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

	dismissQueue := make(chan dismissReq, 128)
	dismissOutcomes := make(chan dismissOutcome, 16)
	go dismissWorker(ctx, client, dismissQueue, dismissOutcomes)

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
				focused = dismissFocused(dismissQueue, st, &cfg, focused)
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
				renderFromState(st, cfg, refreshing, focused)
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
	renderFromState(st, cfg, false, focusTarget{})
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
		pulled, prErr := prs.Poll(client, since)
		res.PRs = pulled
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
		st.PruneDismissed(10 * time.Minute)
		filtered := make([]notifs.Notification, 0, len(res.Notifs))
		for _, n := range res.Notifs {
			if st.IsDismissed(n.ID, n.UpdatedAt) {
				continue
			}
			filtered = append(filtered, n)
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

func renderFromState(st *state.State, cfg watchConfig, refreshing bool, f focusTarget) {
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
