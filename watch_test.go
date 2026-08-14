package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	// Toggle off: an active->failure transition should not alert.
	st.SetFailedBuildAlerts(false)
	applyResult(st, cfg, pollResult{Runs: []runs.Run{{ID: 1, Status: "in_progress"}}})
	applyResult(st, cfg, pollResult{Runs: []runs.Run{
		{ID: 1, Status: "completed", Conclusion: "failure", Repo: "x/y", WorkflowName: "CI", Branch: "main", URL: "https://x/1"},
	}})
	if calls != 0 {
		t.Fatalf("toggle off: want 0 calls, got %d", calls)
	}

	// Toggle on: a fresh run's active->failure transition should alert.
	st.SetFailedBuildAlerts(true)
	applyResult(st, cfg, pollResult{Runs: []runs.Run{{ID: 2, Status: "in_progress"}}})
	applyResult(st, cfg, pollResult{Runs: []runs.Run{
		{ID: 2, Status: "completed", Conclusion: "failure", Repo: "x/y", WorkflowName: "CI", Branch: "main", URL: "https://x/2"},
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

