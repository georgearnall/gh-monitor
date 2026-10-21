package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/georgearnall/gh-monitor/internal/discovery"
	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
	"github.com/georgearnall/gh-monitor/internal/state"
)

// isolateState returns a freshly loaded State backed by a per-test tempdir,
// mirroring internal/state's own isolate(t) helper. applyResult needs a
// State with initialized maps (Runs, DismissedRuns, ...), which only Load
// guarantees.
func isolateState(t *testing.T) *state.State {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return s
}

// discoveryServer returns a test HTTP server that handles the three discovery
// endpoints (user/repos, search/issues, user, users/<login>/events) with empty
// results, and counts how many times /user/repos is hit.
func discoveryServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reposCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user/repos":
			reposCalls.Add(1)
			w.Write([]byte(`[]`))
		case r.URL.Path == "/search/issues":
			w.Write([]byte(`{"items":[]}`))
		case r.URL.Path == "/user":
			w.Write([]byte(`{"login":"testuser"}`))
		default:
			// users/<login>/events and any other endpoints
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reposCalls
}

func TestDoRefresh_DiscoveryCacheHonorsRepoRefresh(t *testing.T) {
	srv, reposCalls := discoveryServer(t)
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	cfg := &watchConfig{
		maxRepos:    20,
		repoRefresh: 5 * time.Minute,
	}

	ctx := context.Background()

	// First call: cache is empty, discovery must run.
	doRefresh(ctx, c, cfg)
	if got := reposCalls.Load(); got != 1 {
		t.Fatalf("first refresh: want 1 discovery call, got %d", got)
	}
	if cfg.lastDiscovery.IsZero() {
		t.Fatal("lastDiscovery should be set after first refresh")
	}

	// Second call immediately after: cache is fresh, discovery must be skipped.
	doRefresh(ctx, c, cfg)
	if got := reposCalls.Load(); got != 1 {
		t.Errorf("second refresh (cache fresh): want still 1 discovery call, got %d", got)
	}

	// Simulate cache expiry by backdating lastDiscovery.
	cfg.lastDiscovery = time.Now().Add(-6 * time.Minute)
	doRefresh(ctx, c, cfg)
	if got := reposCalls.Load(); got != 2 {
		t.Errorf("third refresh (cache expired): want 2 discovery calls, got %d", got)
	}
}

func TestDoRefresh_ZeroRepoRefreshAlwaysDiscovers(t *testing.T) {
	srv, reposCalls := discoveryServer(t)
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	// repoRefresh=0 disables caching: discovery should fire every time.
	cfg := &watchConfig{
		maxRepos:      20,
		repoRefresh:   0,
		cachedRepos:   []discovery.Repo{{FullName: "stale/repo"}},
		lastDiscovery: time.Now(), // fresh-looking cache that should be ignored
	}

	ctx := context.Background()
	doRefresh(ctx, c, cfg)
	doRefresh(ctx, c, cfg)
	if got := reposCalls.Load(); got != 2 {
		t.Errorf("repoRefresh=0 should bypass cache; want 2 calls, got %d", got)
	}
}

func TestWatchConfig_NextInterval(t *testing.T) {
	cfg := watchConfig{
		baseInterval:   60 * time.Second,
		activeInterval: 20 * time.Second,
		lowQuotaFloor:  2 * time.Minute,
		lowQuotaLimit:  500,
	}

	cases := []struct {
		name    string
		active  int
		rl      ghclient.RateLimit
		pollErr error
		want    time.Duration
	}{
		{"idle, no rate-limit info", 0, ghclient.RateLimit{}, nil, 60 * time.Second},
		{"active runs", 2, ghclient.RateLimit{Limit: 5000, Remaining: 4000}, nil, 20 * time.Second},
		{"low quota raises floor", 0, ghclient.RateLimit{Limit: 5000, Remaining: 100}, nil, 2 * time.Minute},
		{"low quota also lifts active", 3, ghclient.RateLimit{Limit: 5000, Remaining: 100}, nil, 2 * time.Minute},
		{"retry-after dominates", 0, ghclient.RateLimit{}, &ghclient.RateLimitedError{RetryAfter: 5 * time.Minute}, 5 * time.Minute},
		{"retry-after small still raised to floor", 0, ghclient.RateLimit{}, &ghclient.RateLimitedError{RetryAfter: 10 * time.Second}, 2 * time.Minute},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cfg.nextInterval(c.active, c.rl, c.pollErr)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestApplyResult_FailedBuildAlert_GatedByToggle(t *testing.T) {
	origFailure := notifyFailure
	defer func() { notifyFailure = origFailure }()
	var calls int
	notifyFailure = func(repo, workflow, branch, url string) error {
		calls++
		return nil
	}

	st := isolateState(t)
	cfg := &watchConfig{}

	st.ViewerLogin = "me"

	// Toggle off: an active->failure transition should not alert.
	st.SetFailedBuildAlerts(false)
	applyResult(st, cfg, pollResult{Runs: []runs.Run{{ID: 1, Status: "in_progress", ActorLogin: "me"}}})
	applyResult(st, cfg, pollResult{Runs: []runs.Run{
		{ID: 1, Status: "completed", Conclusion: "failure", Repo: "x/y", WorkflowName: "CI", Branch: "main", URL: "https://x/1", ActorLogin: "me"},
	}})
	if calls != 0 {
		t.Fatalf("toggle off: want 0 calls, got %d", calls)
	}

	// Toggle on: a fresh run's active->failure transition should alert.
	st.SetFailedBuildAlerts(true)
	applyResult(st, cfg, pollResult{Runs: []runs.Run{{ID: 2, Status: "in_progress", ActorLogin: "me"}}})
	applyResult(st, cfg, pollResult{Runs: []runs.Run{
		{ID: 2, Status: "completed", Conclusion: "failure", Repo: "x/y", WorkflowName: "CI", Branch: "main", URL: "https://x/2", ActorLogin: "me"},
	}})
	if calls != 1 {
		t.Errorf("toggle on: want 1 call, got %d", calls)
	}
}

func TestApplyResult_OwnPRComment_FiresOnce_NotTwice(t *testing.T) {
	origComment, origGeneric := notifyComment, notifyNewNotification
	defer func() { notifyComment, notifyNewNotification = origComment, origGeneric }()
	var commentCalls, genericCalls int
	notifyComment = func(repo string, prNumber int, prTitle, url string) error {
		commentCalls++
		return nil
	}
	notifyNewNotification = func(repo string, prNumber int, reason, title, url string) error {
		genericCalls++
		return nil
	}

	st := isolateState(t)
	st.NotifyOwnPRComments = true
	st.NotifyAllGitHub = true
	cfg := &watchConfig{}

	authored := []prs.PR{{Repo: "x/y", Number: 42}}
	now := time.Now()

	// Warm the ledger with an unrelated notification first so this test
	// isn't exercising cold-start suppression (that's covered separately
	// by TestApplyResult_Bootstrap_NoAlertStormOnFirstToggleOn).
	warmup := notifs.Notification{ID: "warmup", Repo: "a/b", PRNumber: 1, UpdatedAt: now.Add(-time.Hour)}
	applyResult(st, cfg, pollResult{PRs: authored, Notifs: []notifs.Notification{warmup}})

	comment := notifs.Notification{ID: "n1", Repo: "x/y", PRNumber: 42, Title: "hi", UpdatedAt: now, HasCommentAnchor: true}

	// First appearance of the comment notification: should fire, via the
	// own-PR-comment path (not the generic one, even though NotifyAllGitHub
	// is also on).
	applyResult(st, cfg, pollResult{PRs: authored, Notifs: []notifs.Notification{warmup, comment}})
	// Repeat poll, identical notification (same UpdatedAt): must not refire.
	applyResult(st, cfg, pollResult{PRs: authored, Notifs: []notifs.Notification{warmup, comment}})

	if commentCalls != 1 {
		t.Errorf("want 1 notifyComment call, got %d", commentCalls)
	}
	if genericCalls != 0 {
		t.Errorf("want 0 notifyNewNotification calls (own-PR-comment path should claim it), got %d", genericCalls)
	}
}

func TestApplyResult_Bootstrap_NoAlertStormOnFirstToggleOn(t *testing.T) {
	origGeneric := notifyNewNotification
	defer func() { notifyNewNotification = origGeneric }()
	var calls int
	notifyNewNotification = func(repo string, prNumber int, reason, title, url string) error {
		calls++
		return nil
	}

	st := isolateState(t)
	st.NotifyAllGitHub = true
	cfg := &watchConfig{}
	now := time.Now()

	// A notification already sitting in the inbox the very first time
	// gh-monitor (or this toggle) ever polls: must not alert.
	n := notifs.Notification{ID: "old1", Repo: "a/b", PRNumber: 5, Title: "pre-existing", Reason: "mention", UpdatedAt: now}
	applyResult(st, cfg, pollResult{Notifs: []notifs.Notification{n}})
	if calls != 0 {
		t.Fatalf("bootstrap poll: want 0 calls, got %d", calls)
	}

	// Genuinely new activity on that same thread afterwards: must alert.
	n.UpdatedAt = now.Add(1 * time.Hour)
	applyResult(st, cfg, pollResult{Notifs: []notifs.Notification{n}})
	if calls != 1 {
		t.Errorf("later update to the same thread: want 1 call, got %d", calls)
	}
}

func TestApplyResult_NoNotify_SuppressesAllThreeAlertTypes(t *testing.T) {
	origFailure, origComment, origGeneric := notifyFailure, notifyComment, notifyNewNotification
	defer func() { notifyFailure, notifyComment, notifyNewNotification = origFailure, origComment, origGeneric }()
	var failureCalls, commentCalls, genericCalls int
	notifyFailure = func(repo, workflow, branch, url string) error { failureCalls++; return nil }
	notifyComment = func(repo string, prNumber int, prTitle, url string) error { commentCalls++; return nil }
	notifyNewNotification = func(repo string, prNumber int, reason, title, url string) error { genericCalls++; return nil }

	st := isolateState(t)
	st.NotifyAllGitHub = true
	st.NotifyOwnPRComments = true
	cfg := &watchConfig{noNotify: true}

	authored := []prs.PR{{Repo: "x/y", Number: 42}}
	now := time.Now()
	warmup := notifs.Notification{ID: "warmup", Repo: "a/b", PRNumber: 1, UpdatedAt: now.Add(-time.Hour)}
	comment := notifs.Notification{ID: "n1", Repo: "x/y", PRNumber: 42, Title: "hi", UpdatedAt: now, HasCommentAnchor: true}

	applyResult(st, cfg, pollResult{PRs: authored, Notifs: []notifs.Notification{warmup}})
	applyResult(st, cfg, pollResult{Runs: []runs.Run{{ID: 1, Status: "in_progress"}}})
	applyResult(st, cfg, pollResult{
		Runs: []runs.Run{
			{ID: 1, Status: "completed", Conclusion: "failure", Repo: "x/y", WorkflowName: "CI", Branch: "main", URL: "https://x/1"},
		},
		PRs:    authored,
		Notifs: []notifs.Notification{warmup, comment},
	})

	if failureCalls != 0 || commentCalls != 0 || genericCalls != 0 {
		t.Errorf("--no-notify should suppress every alert type, got failure=%d comment=%d generic=%d",
			failureCalls, commentCalls, genericCalls)
	}
}

func TestApplyResult_FailedBuildAlert_OnlyViewersOwnRuns(t *testing.T) {
	origFailure := notifyFailure
	defer func() { notifyFailure = origFailure }()
	var alerted []string
	notifyFailure = func(repo, workflow, branch, url string) error {
		alerted = append(alerted, url)
		return nil
	}

	st := isolateState(t)
	cfg := &watchConfig{}
	active := []runs.Run{
		{ID: 1, Status: "in_progress", ActorLogin: "me", URL: "mine"},
		{ID: 2, Status: "in_progress", ActorLogin: "colleague", URL: "colleague"},
		{ID: 3, Status: "in_progress", ActorLogin: "renovate[bot]", Branch: "renovate/x", URL: "bot"},
	}
	applyResult(st, cfg, pollResult{Runs: active, ViewerLogin: "me"})
	failed := make([]runs.Run, len(active))
	for i, r := range active {
		r.Status, r.Conclusion = "completed", "failure"
		failed[i] = r
	}
	applyResult(st, cfg, pollResult{Runs: failed, ViewerLogin: "me"})
	if len(alerted) != 1 || alerted[0] != "mine" {
		t.Errorf("want only the viewer's run to alert, got %v", alerted)
	}
}

func TestDoRefresh_DiscoveryFailureFallsBackToCachedRepos(t *testing.T) {
	var runsCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user/repos":
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/user":
			w.Write([]byte(`{"login":"testuser"}`))
		case r.URL.Path == "/graphql":
			w.Write([]byte(`{"data":{}}`))
		case r.URL.Path == "/repos/o/cached/actions/runs":
			runsCalls.Add(1)
			w.Write([]byte(`{"workflow_runs":[]}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	cfg := &watchConfig{
		maxRepos:    20,
		repoRefresh: time.Minute,
		cachedRepos: []discovery.Repo{{FullName: "o/cached", Owner: "o", Name: "cached"}},
	}
	res := doRefresh(context.Background(), c, cfg)
	if res.DiscErr == nil {
		t.Fatal("expected discovery error to be reported")
	}
	if !res.RunsPolled || runsCalls.Load() != 1 {
		t.Errorf("runs should be polled against the cached repo list (polled=%v calls=%d)", res.RunsPolled, runsCalls.Load())
	}
	if len(res.Repos) != 1 || res.Repos[0].FullName != "o/cached" {
		t.Errorf("Repos = %+v, want cached list", res.Repos)
	}
}

func TestMuteFocusedRepo_ScrubsAllPanelsAndKeepsCursorPosition(t *testing.T) {
	st := isolateState(t)
	st.ViewerLogin = "me"
	st.LastNotifs = []notifs.Notification{
		{ID: "n1", Repo: "o/keep", Unread: true},
		{ID: "n2", Repo: "o/noisy", Unread: true},
		{ID: "n3", Repo: "o/keep", Unread: true},
	}
	st.LastPRs = []prs.PR{{Repo: "o/noisy", Number: 1}, {Repo: "o/keep", Number: 2}}
	st.LastAssignedPRs = []prs.PR{{Repo: "o/noisy", Number: 3}}
	st.LastView = []runs.Run{
		{ID: 10, Repo: "o/noisy", ActorLogin: "me", Status: "in_progress", UpdatedAt: time.Now()},
		{ID: 11, Repo: "o/keep", ActorLogin: "me", Status: "in_progress", UpdatedAt: time.Now()},
	}
	cfg := &watchConfig{muted: newRepoSet(nil)}

	focus, msg := muteFocusedRepo(st, cfg, focusTarget{"notifs", "n2"})

	if !st.MutedRepos["o/noisy"] || !cfg.muted.Load()["o/noisy"] {
		t.Fatal("repo should be muted in state and in the producer snapshot")
	}
	for _, n := range st.LastNotifs {
		if n.Repo == "o/noisy" {
			t.Errorf("muted notification left: %+v", n)
		}
	}
	if len(st.LastPRs) != 1 || st.LastPRs[0].Repo != "o/keep" || len(st.LastAssignedPRs) != 0 {
		t.Errorf("muted PRs left: %+v / %+v", st.LastPRs, st.LastAssignedPRs)
	}
	if len(st.LastView) != 1 || st.LastView[0].Repo != "o/keep" {
		t.Errorf("muted runs left (including the viewer's own): %+v", st.LastView)
	}
	if focus != (focusTarget{"notifs", "n3"}) {
		t.Errorf("focus = %+v, want the row that moved into n2's position (n3)", focus)
	}
	if !strings.Contains(msg, "o/noisy") {
		t.Errorf("status message %q should name the muted repo", msg)
	}
}

func TestApplyResult_MutedRepoHiddenEverywhereAndSilent(t *testing.T) {
	origFailure, origGeneric := notifyFailure, notifyNewNotification
	defer func() { notifyFailure, notifyNewNotification = origFailure, origGeneric }()
	var alerts int
	notifyFailure = func(_, _, _, _ string) error { alerts++; return nil }
	notifyNewNotification = func(_ string, _ int, _, _, _ string) error { alerts++; return nil }

	st := isolateState(t)
	st.NotifyAllGitHub = true
	st.MuteRepo("o/noisy")
	st.AlertedNotifs = map[string]state.NotifAlertRecord{"warm": {}} // not a cold start
	cfg := &watchConfig{}
	now := time.Now()

	poll := func(conclusion string) pollResult {
		status := "in_progress"
		if conclusion != "" {
			status = "completed"
		}
		return pollResult{
			ViewerLogin: "me",
			RunsPolled:  true,
			Repos:       []discovery.Repo{{FullName: "o/keep"}},
			Runs:        []runs.Run{{ID: 1, Repo: "o/noisy", ActorLogin: "me", Status: status, Conclusion: conclusion, UpdatedAt: now}},
			PRs:         []prs.PR{{Repo: "o/noisy", Number: 1}, {Repo: "o/keep", Number: 2}},
			Notifs:      []notifs.Notification{{ID: "n1", Repo: "o/noisy", Unread: true, UpdatedAt: now}},
		}
	}
	applyResult(st, cfg, poll(""))
	applyResult(st, cfg, poll("failure"))

	if alerts != 0 {
		t.Errorf("muted repo fired %d alerts, want 0", alerts)
	}
	if len(st.LastView) != 0 || len(st.LastNotifs) != 0 {
		t.Errorf("muted rows shown: runs=%+v notifs=%+v", st.LastView, st.LastNotifs)
	}
	if len(st.LastPRs) != 1 || st.LastPRs[0].Repo != "o/keep" {
		t.Errorf("LastPRs = %+v, want only o/keep", st.LastPRs)
	}
	if _, ok := st.AlertedNotifs["n1"]; !ok {
		t.Error("muted notification should still be recorded so unmuting doesn't alert on it")
	}
}

func TestDoRefresh_MutedRepoNotPolled(t *testing.T) {
	var mutedCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			w.Write([]byte(`{"login":"me"}`))
		case r.URL.Path == "/graphql":
			w.Write([]byte(`{"data":{}}`))
		case strings.HasPrefix(r.URL.Path, "/repos/o/noisy/"):
			mutedCalls.Add(1)
			w.Write([]byte(`{"workflow_runs":[]}`))
		default:
			w.Write([]byte(`{"workflow_runs":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	cfg := &watchConfig{
		maxRepos:      20,
		repoRefresh:   time.Hour,
		lastDiscovery: time.Now(),
		cachedRepos:   []discovery.Repo{{FullName: "o/noisy", Owner: "o", Name: "noisy"}, {FullName: "o/keep", Owner: "o", Name: "keep"}},
		muted:         newRepoSet(map[string]bool{"o/noisy": true}),
	}
	res := doRefresh(context.Background(), c, cfg)
	if mutedCalls.Load() != 0 {
		t.Errorf("muted repo was polled %d times", mutedCalls.Load())
	}
	if len(res.Repos) != 1 || res.Repos[0].FullName != "o/keep" {
		t.Errorf("Repos = %+v, want only o/keep", res.Repos)
	}
}
