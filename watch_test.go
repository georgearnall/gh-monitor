package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
)

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

