package main

import (
	"context"
	"fmt"
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
	notifyFailure = func(repo, workflow, branch, failedAt, url string) error {
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
	notifyFailure = func(repo, workflow, branch, failedAt, url string) error { failureCalls++; return nil }
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
	notifyFailure = func(repo, workflow, branch, failedAt, url string) error {
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
	notifyFailure = func(_, _, _, _, _ string) error { alerts++; return nil }
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

func TestConfigRepoRows_MonitoredThenAllMuted(t *testing.T) {
	st := isolateState(t)
	now := time.Now()
	st.Repos = []discovery.Repo{
		{FullName: "o/old", Activity: now.Add(-time.Hour)},
		{FullName: "o/new", Activity: now},
	}
	st.MuteRepo("o/z-gone") // muted but no longer discovered
	st.MuteRepo("o/a-gone")
	rows := configRepoRows(st)
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s:%v", r.Name, r.Muted))
	}
	want := "o/new:false,o/old:false,o/a-gone:true,o/z-gone:true"
	if strings.Join(got, ",") != want {
		t.Errorf("rows = %s, want %s", strings.Join(got, ","), want)
	}
}

func TestToggleConfigRepoMute_MuteThenUnmute(t *testing.T) {
	st := isolateState(t)
	st.Repos = []discovery.Repo{
		{FullName: "o/a", Activity: time.Now()},
		{FullName: "o/b", Activity: time.Now().Add(-time.Hour)},
	}
	st.LastPRs = []prs.PR{{Repo: "o/a", Number: 1}}
	cfg := &watchConfig{muted: newRepoSet(nil)}

	// Muting moves o/a to the muted section; the cursor follows it.
	focus, ok := toggleConfigRepoMute(st, cfg, "o/a")
	if !ok || !st.MutedRepos["o/a"] || !cfg.muted.Load()["o/a"] {
		t.Fatal("first toggle should mute o/a in state and producer snapshot")
	}
	if focus != "o/a" {
		t.Errorf("cursor should follow the repo it toggled, got %q", focus)
	}
	if len(st.LastPRs) != 0 {
		t.Errorf("muting from config should scrub rows, LastPRs = %+v", st.LastPRs)
	}

	// Unmuting a repo discovery still returns keeps it listed and focused.
	if focus, ok = toggleConfigRepoMute(st, cfg, focus); !ok || st.MutedRepos["o/a"] || cfg.muted.Load()["o/a"] {
		t.Error("second toggle should unmute o/a")
	}
	if focus != "o/a" {
		t.Errorf("cursor should stay on o/a after unmute, got %q", focus)
	}
}

func TestToggleConfigRepoMute_UnmuteUndiscoveredRepoMovesCursorToNeighbour(t *testing.T) {
	st := isolateState(t)
	st.Repos = []discovery.Repo{{FullName: "o/live", Activity: time.Now()}}
	st.MuteRepo("o/gone-a")
	st.MuteRepo("o/gone-b")
	cfg := &watchConfig{muted: newRepoSet(st.MutedRepos)}

	// Rows: o/live, o/gone-a, o/gone-b. Unmuting o/gone-a removes its row
	// (discovery doesn't return it), so the cursor lands on o/gone-b.
	focus, _ := toggleConfigRepoMute(st, cfg, "o/gone-a")
	if focus != "o/gone-b" {
		t.Errorf("focus = %q, want o/gone-b", focus)
	}
}

func TestMuteThenUnmute_NoStaleFailureAlert(t *testing.T) {
	origFailure := notifyFailure
	defer func() { notifyFailure = origFailure }()
	var alerts int
	notifyFailure = func(_, _, _, _, _ string) error { alerts++; return nil }

	st := isolateState(t)
	cfg := &watchConfig{muted: newRepoSet(nil)}
	active := runs.Run{ID: 1, Repo: "o/a", ActorLogin: "me", Status: "in_progress", UpdatedAt: time.Now()}
	applyResult(st, cfg, pollResult{Runs: []runs.Run{active}, RunsPolled: true, ViewerLogin: "me"})

	muteFocusedRepo(st, cfg, focusTarget{"runs", "1"})
	st.UnmuteRepo("o/a")

	// First poll after unmuting: the run finished (failed) while muted.
	done := active
	done.Status, done.Conclusion = "completed", "failure"
	applyResult(st, cfg, pollResult{Runs: []runs.Run{done}, RunsPolled: true, ViewerLogin: "me"})
	if alerts != 0 {
		t.Errorf("failure that happened while muted alerted %d times on unmute", alerts)
	}
}

// progressServer serves the jobs endpoint for every run, counting calls per
// run ID. failing makes it 502; remaining sets X-RateLimit-Remaining.
type progressServer struct {
	calls     map[string]int
	failing   atomic.Bool
	remaining atomic.Int32
	mu        chan struct{}
}

func newProgressServer(t *testing.T) (*progressServer, *ghclient.Client) {
	t.Helper()
	ps := &progressServer{calls: map[string]int{}, mu: make(chan struct{}, 1)}
	ps.remaining.Store(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(ps.remaining.Load()))
		ps.mu <- struct{}{}
		ps.calls[r.URL.Path]++
		<-ps.mu
		if ps.failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"jobs":[{"name":"build","status":"completed","conclusion":"failure","steps":[{"name":"Compile","status":"completed","conclusion":"failure"}]},{"name":"deploy","status":"in_progress","steps":[{"name":"Push","status":"in_progress"}]}]}`))
	}))
	t.Cleanup(srv.Close)
	return ps, ghclient.NewForTest(srv.Client(), srv.URL+"/")
}

func (ps *progressServer) count(id int64) int {
	ps.mu <- struct{}{}
	defer func() { <-ps.mu }()
	return ps.calls[fmt.Sprintf("/repos/o/a/actions/runs/%d/jobs", id)]
}

func TestAttachProgress_ActiveRunGetsProgress(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	rs := []runs.Run{{ID: 1, Repo: "o/a", Status: "in_progress", ActorLogin: "someone", UpdatedAt: time.Now()}}
	attachProgress(c, cfg, rs, "me")
	if rs[0].Progress == nil || rs[0].Progress.Job != "deploy" || rs[0].Progress.Step != "Push" {
		t.Fatalf("progress = %+v", rs[0].Progress)
	}
	attachProgress(c, cfg, rs, "me")
	if n := ps.count(1); n != 2 {
		t.Errorf("active runs are refetched every poll: got %d calls, want 2", n)
	}
}

func TestAttachProgress_FailedRunFetchedOnce(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	mk := func() []runs.Run {
		return []runs.Run{{ID: 2, Repo: "o/a", Status: "completed", Conclusion: "failure", ActorLogin: "me", UpdatedAt: time.Now()}}
	}
	rs := mk()
	attachProgress(c, cfg, rs, "me")
	rs = mk()
	attachProgress(c, cfg, rs, "me")
	if n := ps.count(2); n != 1 {
		t.Errorf("completed run fetched %d times, want 1", n)
	}
	if rs[0].Progress == nil || rs[0].Progress.FailedStep != "Compile" {
		t.Errorf("cached progress not attached: %+v", rs[0].Progress)
	}
}

func TestAttachProgress_RunCompletingIsRefetched(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	attachProgress(c, cfg, []runs.Run{{ID: 3, Repo: "o/a", Status: "in_progress", ActorLogin: "me", UpdatedAt: time.Now()}}, "me")
	attachProgress(c, cfg, []runs.Run{{ID: 3, Repo: "o/a", Status: "completed", Conclusion: "failure", ActorLogin: "me", UpdatedAt: time.Now()}}, "me")
	if n := ps.count(3); n != 2 {
		t.Errorf("a run that just completed must be refetched for its final jobs: got %d calls", n)
	}
}

func TestAttachProgress_ErrorKeepsPrevious(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	attachProgress(c, cfg, []runs.Run{{ID: 1, Repo: "o/a", Status: "in_progress", UpdatedAt: time.Now()}}, "me")
	ps.failing.Store(true)
	rs := []runs.Run{{ID: 1, Repo: "o/a", Status: "in_progress", UpdatedAt: time.Now()}}
	attachProgress(c, cfg, rs, "me")
	if rs[0].Progress == nil || rs[0].Progress.Job != "deploy" {
		t.Errorf("transient error must keep the last progress, got %+v", rs[0].Progress)
	}
}

func TestAttachProgress_LowQuotaSkipsFetches(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	ps.remaining.Store(100)
	attachProgress(c, cfg, []runs.Run{{ID: 1, Repo: "o/a", Status: "in_progress", UpdatedAt: time.Now()}}, "me")
	// That call recorded the low quota; the next must not fetch.
	attachProgress(c, cfg, []runs.Run{{ID: 1, Repo: "o/a", Status: "in_progress", UpdatedAt: time.Now()}}, "me")
	if n := ps.count(1); n != 1 {
		t.Errorf("low quota should skip fetches: got %d calls, want 1", n)
	}
}

func TestAttachProgress_SkipsOthersCompletedAndBots(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	rs := []runs.Run{
		{ID: 4, Repo: "o/a", Status: "completed", Conclusion: "failure", ActorLogin: "someone", UpdatedAt: time.Now()},
		{ID: 5, Repo: "o/a", Status: "in_progress", ActorLogin: "renovate[bot]", UpdatedAt: time.Now()},
		{ID: 6, Repo: "o/a", Status: "completed", Conclusion: "success", ActorLogin: "me", UpdatedAt: time.Now()},
	}
	attachProgress(c, cfg, rs, "me")
	for _, id := range []int64{4, 5, 6} {
		if n := ps.count(id); n != 0 {
			t.Errorf("run %d fetched %d times, want 0", id, n)
		}
	}
}

func TestApplyResult_FailureAlertIncludesFailedStep(t *testing.T) {
	origFailure := notifyFailure
	defer func() { notifyFailure = origFailure }()
	var got []string
	notifyFailure = func(_, _, _, failedAt, _ string) error {
		got = append(got, failedAt)
		return nil
	}
	st := isolateState(t)
	st.ViewerLogin = "me"
	cfg := &watchConfig{}
	applyResult(st, cfg, pollResult{Runs: []runs.Run{{ID: 1, Status: "in_progress", ActorLogin: "me"}}})
	applyResult(st, cfg, pollResult{Runs: []runs.Run{{ID: 1, Status: "completed", Conclusion: "failure", Repo: "x/y", ActorLogin: "me",
		Progress: &runs.Progress{FailedJob: "deploy-prod", FailedStep: "Migrate DB"}}}})
	if len(got) != 1 || got[0] != "deploy-prod › Migrate DB" {
		t.Errorf("failedAt passed to alert = %q", got)
	}
}

func TestAttachProgress_ManyCachedRunsNoRace(t *testing.T) {
	_, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	var rs []runs.Run
	for i := int64(1); i <= 10; i++ {
		rs = append(rs, runs.Run{ID: i, Repo: "o/a", Status: "in_progress", UpdatedAt: time.Now()})
	}
	// More runs than progressConcurrency: once workers are in flight, later
	// iterations must not touch the shared map while copying the cache.
	for range 3 {
		attachProgress(c, cfg, append([]runs.Run(nil), rs...), "me")
	}
}

func TestAttachProgress_RerunRefetchesFinalRun(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	failed := runs.Run{ID: 7, Repo: "o/a", Status: "completed", Conclusion: "failure", ActorLogin: "me", UpdatedAt: time.Now(), RunAttempt: 1}
	attachProgress(c, cfg, []runs.Run{failed}, "me")

	// Re-running keeps the ID and bumps the attempt; the cached failure
	// from attempt 1 must not be reused or shown.
	ps.failing.Store(true)
	rerun := runs.Run{ID: 7, Repo: "o/a", Status: "in_progress", ActorLogin: "me", UpdatedAt: time.Now(), RunAttempt: 2}
	rs := []runs.Run{rerun}
	attachProgress(c, cfg, rs, "me")
	if n := ps.count(7); n != 2 {
		t.Errorf("re-run should be refetched: got %d calls, want 2", n)
	}
	if rs[0].Progress != nil {
		t.Errorf("attempt 1's progress must not be shown for attempt 2: %+v", rs[0].Progress)
	}
}

func TestAttachProgress_DismissedRunsDontTakeSlots(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200, dismissedRuns: newRunSet(map[int64]bool{1: true})}
	now := time.Now()
	var rs []runs.Run
	// 11 active runs, newest first. The panel caps at 10 after dropping the
	// dismissed run 1, so it shows 2..11, and those are what get progress.
	for i := int64(1); i <= 11; i++ {
		rs = append(rs, runs.Run{ID: i, Repo: "o/a", Status: "in_progress", UpdatedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	attachProgress(c, cfg, rs, "me")
	if n := ps.count(1); n != 0 {
		t.Errorf("dismissed run fetched %d times, want 0", n)
	}
	if n := ps.count(11); n != 1 {
		t.Errorf("oldest shown run fetched %d times, want 1", n)
	}
}

func TestDismissFocusedRun_UpdatesProducerSet(t *testing.T) {
	st := isolateState(t)
	st.LastView = []runs.Run{{ID: 5, Repo: "o/a", Status: "in_progress", UpdatedAt: time.Now()}}
	cfg := &watchConfig{dismissedRuns: newRunSet(nil)}
	dismissFocusedRun(st, cfg, focusTarget{Panel: "runs", ID: "5"})
	if !cfg.dismissedRuns.Load()[5] {
		t.Error("dismissing a run must publish it to the producer's set")
	}
}

func TestAttachProgress_RateLimitPausesFetches(t *testing.T) {
	var calls atomic.Int32
	var limited atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if limited.Load() {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"jobs":[{"name":"deploy","status":"in_progress"}]}`))
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	cfg := &watchConfig{lowQuotaLimit: 200, lowQuotaFloor: 2 * time.Minute, baseInterval: time.Minute, activeInterval: 20 * time.Second}
	mk := func() []runs.Run { return []runs.Run{{ID: 1, Repo: "o/a", Status: "in_progress", UpdatedAt: time.Now()}} }

	if err := attachProgress(c, cfg, mk(), "me"); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	limited.Store(true)
	rs := mk()
	err := attachProgress(c, cfg, rs, "me")
	if _, ok := ghclient.AsRateLimited(err); !ok {
		t.Fatalf("want the rate limit returned, got %v", err)
	}
	if rs[0].Progress == nil || rs[0].Progress.Job != "deploy" {
		t.Errorf("rate limit must keep the cached progress, got %+v", rs[0].Progress)
	}
	if d := cfg.nextInterval(1, ghclient.RateLimit{}, err); d < 120*time.Second {
		t.Errorf("next poll in %s, want at least the 120s Retry-After", d)
	}

	before := calls.Load()
	limited.Store(false)
	rs = mk()
	if err := attachProgress(c, cfg, rs, "me"); err != nil {
		t.Fatalf("paused pass: %v", err)
	}
	if calls.Load() != before {
		t.Error("no progress fetches should be made during the cooldown")
	}
	if rs[0].Progress == nil {
		t.Error("cached progress should still show during the cooldown")
	}
}

func TestApplyResult_PrunedDismissalReachesProducer(t *testing.T) {
	st := isolateState(t)
	st.DismissedRuns[9] = time.Now().Add(-2 * time.Minute)
	cfg := &watchConfig{dismissedRuns: newRunSet(dismissedRunSet(st))}
	// Run 9 is gone from a successful poll, so its old dismissal is pruned.
	applyResult(st, cfg, pollResult{RunsPolled: true, Runs: []runs.Run{{ID: 1, Status: "in_progress"}}})
	if st.IsRunDismissed(9) {
		t.Fatal("precondition: dismissal should have been pruned from state")
	}
	if cfg.dismissedRuns.Load()[9] {
		t.Error("the producer still excludes a run whose dismissal was pruned")
	}
}

func TestAttachProgress_KeepsCacheForRunsPushedOutOfView(t *testing.T) {
	ps, c := newProgressServer(t)
	cfg := &watchConfig{lowQuotaLimit: 200}
	now := time.Now()
	failed := runs.Run{ID: 100, Repo: "o/a", Status: "completed", Conclusion: "failure", ActorLogin: "me", UpdatedAt: now.Add(-time.Hour)}
	attachProgress(c, cfg, []runs.Run{failed}, "me")

	// Ten newer active runs push the failed run out of the visible rows.
	busy := []runs.Run{failed}
	for i := int64(1); i <= 10; i++ {
		busy = append(busy, runs.Run{ID: i, Repo: "o/a", Status: "in_progress", UpdatedAt: now})
	}
	attachProgress(c, cfg, busy, "me")

	// Back in view: served from cache, not refetched.
	rs := []runs.Run{failed}
	attachProgress(c, cfg, rs, "me")
	if n := ps.count(100); n != 1 {
		t.Errorf("failed run fetched %d times, want 1", n)
	}
	if rs[0].Progress == nil || rs[0].Progress.FailedStep != "Compile" {
		t.Errorf("cached failure location lost: %+v", rs[0].Progress)
	}
}
