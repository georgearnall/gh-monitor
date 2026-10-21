package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/georgearnall/gh-monitor/internal/discovery"
	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"github.com/georgearnall/gh-monitor/internal/state"
)

func mkState(t *testing.T) *state.State {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return s
}

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

func TestSelectRepos_Excludes(t *testing.T) {
	repos := []discovery.Repo{
		{FullName: "a/x"},
		{FullName: "a/y"},
		{FullName: "b/z"},
	}
	got := selectRepos(repos, stringSet{"a/y": true}, nil, 0)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	for _, r := range got {
		if r.FullName == "a/y" {
			t.Errorf("excluded repo leaked: %v", got)
		}
	}
}

func TestSelectRepos_NoExclusions(t *testing.T) {
	repos := []discovery.Repo{{FullName: "a/b"}}
	got := selectRepos(repos, nil, nil, 0)
	if len(got) != 1 {
		t.Errorf("nil exclusion set should be a no-op, got %+v", got)
	}
}

// Regression: filtering used to happen in place on the discovery cache, so
// every cached refresh duplicated the tail ([a,b,c] minus b -> [a,c,c]).
func TestSelectRepos_DoesNotMutateInput(t *testing.T) {
	repos := []discovery.Repo{{FullName: "a/a"}, {FullName: "a/b"}, {FullName: "a/c"}}
	for i := 0; i < 3; i++ {
		got := selectRepos(repos, stringSet{"a/b": true}, nil, 0)
		if len(got) != 2 || got[0].FullName != "a/a" || got[1].FullName != "a/c" {
			t.Fatalf("pass %d: got %+v", i, got)
		}
	}
	if repos[1].FullName != "a/b" || repos[2].FullName != "a/c" {
		t.Errorf("input mutated: %+v", repos)
	}
}

func TestSelectRepos_CapAppliesAfterExclusionsAndMutes(t *testing.T) {
	repos := []discovery.Repo{{FullName: "a/a"}, {FullName: "a/b"}, {FullName: "a/c"}}
	got := selectRepos(repos, stringSet{"a/a": true}, map[string]bool{"a/b": true}, 1)
	if len(got) != 1 || got[0].FullName != "a/c" {
		t.Errorf("excluded and muted repos must not take capped places: want [a/c], got %+v", got)
	}
}

func TestAsRateLimited_ReturnsFalseForPlainError(t *testing.T) {
	err := errors.New("plain")
	if _, ok := ghclient.AsRateLimited(err); ok {
		t.Errorf("AsRateLimited returned true for non-RateLimitedError")
	}
}
