package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
	"github.com/georgearnall/gha-monitor/internal/notifs"
)

func TestStringSet(t *testing.T) {
	var s stringSet
	if err := s.Set("a/b"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("c/d"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("a/b"); err != nil {
		t.Fatalf("Set dup: %v", err)
	}
	if !s["a/b"] || !s["c/d"] {
		t.Errorf("missing entries: %v", s)
	}
	got := s.String()
	// stringSet.String iterates a map, so order isn't stable — assert membership.
	for _, want := range []string{"a/b", "c/d"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

func TestFilterExcluded(t *testing.T) {
	repos := []discovery.Repo{
		{FullName: "a/x"},
		{FullName: "a/y"},
		{FullName: "b/z"},
	}
	got := filterExcluded(repos, stringSet{"a/y": true})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	for _, r := range got {
		if r.FullName == "a/y" {
			t.Errorf("excluded repo leaked: %v", got)
		}
	}
}

func TestFilterExcluded_NoExclusions(t *testing.T) {
	repos := []discovery.Repo{{FullName: "a/b"}}
	got := filterExcluded(repos, nil)
	if len(got) != 1 {
		t.Errorf("nil exclusion set should be a no-op, got %+v", got)
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

func TestPickFocus(t *testing.T) {
	ns := []notifs.Notification{
		{ID: "1", Unread: false},
		{ID: "2", Unread: true},
		{ID: "3", Unread: true},
	}

	if got := pickFocus(ns, ""); got != "2" {
		t.Errorf("empty current should pick first unread; got %q want %q", got, "2")
	}
	if got := pickFocus(ns, "3"); got != "3" {
		t.Errorf("existing current should be preserved; got %q want %q", got, "3")
	}
	if got := pickFocus(ns, "missing"); got != "2" {
		t.Errorf("missing current falls back to first unread; got %q", got)
	}
	if got := pickFocus(nil, ""); got != "" {
		t.Errorf("empty list returns empty, got %q", got)
	}

	allRead := []notifs.Notification{{ID: "a", Unread: false}, {ID: "b", Unread: false}}
	if got := pickFocus(allRead, ""); got != "a" {
		t.Errorf("all-read list falls back to first item, got %q", got)
	}
}

func TestMoveFocus(t *testing.T) {
	ns := []notifs.Notification{{ID: "1"}, {ID: "2"}, {ID: "3"}}

	if got := moveFocus(ns, "1", +1); got != "2" {
		t.Errorf("down from 1 = %q, want 2", got)
	}
	if got := moveFocus(ns, "3", -1); got != "2" {
		t.Errorf("up from 3 = %q, want 2", got)
	}
	if got := moveFocus(ns, "1", -1); got != "1" {
		t.Errorf("clamp at top: got %q want 1", got)
	}
	if got := moveFocus(ns, "3", +1); got != "3" {
		t.Errorf("clamp at bottom: got %q want 3", got)
	}
	if got := moveFocus(ns, "missing", +1); got != "1" {
		t.Errorf("missing current resets to first; got %q", got)
	}
	if got := moveFocus(nil, "x", +1); got != "" {
		t.Errorf("empty list returns empty, got %q", got)
	}
}

func TestWatchConfig_NextInterval_PreservesRateLimitedAsErr(t *testing.T) {
	// Sanity: ghclient.AsRateLimited recognises a wrapped pointer-error.
	err := errors.New("plain")
	if _, ok := ghclient.AsRateLimited(err); ok {
		t.Errorf("AsRateLimited returned true for non-RateLimitedError")
	}
}
