package discovery

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/georgearnall/gha-monitor/internal/ghclient"
)

func TestMergeRepos(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t0.Add(2 * time.Hour)

	cases := []struct {
		name string
		a, b []Repo
		want map[string]time.Time // full_name -> expected Activity
	}{
		{
			name: "empty inputs",
			a:    nil,
			b:    nil,
			want: map[string]time.Time{},
		},
		{
			name: "only a",
			a:    []Repo{{FullName: "x/y", Activity: t0}},
			b:    nil,
			want: map[string]time.Time{"x/y": t0},
		},
		{
			name: "only b",
			a:    nil,
			b:    []Repo{{FullName: "x/y", Activity: t0}},
			want: map[string]time.Time{"x/y": t0},
		},
		{
			name: "disjoint union",
			a:    []Repo{{FullName: "x/a", Activity: t0}},
			b:    []Repo{{FullName: "x/b", Activity: t1}},
			want: map[string]time.Time{"x/a": t0, "x/b": t1},
		},
		{
			name: "overlap, b is newer — b wins",
			a:    []Repo{{FullName: "x/y", Activity: t0}},
			b:    []Repo{{FullName: "x/y", Activity: t2}},
			want: map[string]time.Time{"x/y": t2},
		},
		{
			name: "overlap, a is newer — a wins (b doesn't regress)",
			a:    []Repo{{FullName: "x/y", Activity: t2}},
			b:    []Repo{{FullName: "x/y", Activity: t0}},
			want: map[string]time.Time{"x/y": t2},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeRepos(c.a, c.b)
			if len(got) != len(c.want) {
				t.Fatalf("len=%d want %d (%+v)", len(got), len(c.want), got)
			}
			// Order is map-iteration so sort for stability.
			sort.Slice(got, func(i, j int) bool { return got[i].FullName < got[j].FullName })
			for _, r := range got {
				want, ok := c.want[r.FullName]
				if !ok {
					t.Errorf("unexpected repo %q", r.FullName)
					continue
				}
				if !r.Activity.Equal(want) {
					t.Errorf("%s activity=%v want %v", r.FullName, r.Activity, want)
				}
			}
		})
	}
}

func TestDiscover_UnionsThreeSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"georgearnall"}`)

		case strings.HasPrefix(r.URL.Path, "/user/repos"):
			// One repo only available via /user/repos
			fmt.Fprint(w, `[
				{"full_name":"x/from-user-repos","owner":{"login":"x"},"name":"from-user-repos","pushed_at":"2026-05-18T10:00:00Z","html_url":"https://github.com/x/from-user-repos"}
			]`)

		case strings.HasPrefix(r.URL.Path, "/search/issues"):
			fmt.Fprint(w, `{"items":[
				{"repository_url":"https://api.github.com/repos/x/from-pr-search","updated_at":"2026-05-18T11:00:00Z"}
			]}`)

		case strings.HasPrefix(r.URL.Path, "/users/georgearnall/events"):
			// Team-membership-only repo: only visible via events
			fmt.Fprint(w, `[
				{"type":"PushEvent","repo":{"name":"x/from-events"},"created_at":"2026-05-18T12:00:00Z"},
				{"type":"ReleaseEvent","repo":{"name":"x/from-user-repos"},"created_at":"2026-05-18T13:00:00Z"}
			]`)

		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	repos, err := Discover(c, 20)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	want := map[string]time.Time{
		"x/from-user-repos": time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC), // events wins (newer)
		"x/from-pr-search":  time.Date(2026, 5, 18, 11, 0, 0, 0, time.UTC),
		"x/from-events":     time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC),
	}
	if len(repos) != len(want) {
		t.Fatalf("got %d repos, want %d: %+v", len(repos), len(want), repos)
	}
	for _, r := range repos {
		ts, ok := want[r.FullName]
		if !ok {
			t.Errorf("unexpected repo %s", r.FullName)
			continue
		}
		if !r.Activity.Equal(ts) {
			t.Errorf("%s activity=%v, want %v", r.FullName, r.Activity, ts)
		}
	}
	// Sort order: most recent activity first.
	if repos[0].FullName != "x/from-user-repos" {
		t.Errorf("first repo should be most-recently-active, got %s", repos[0].FullName)
	}
}

func TestDiscover_RespectsMaxRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"u"}`)
		case strings.HasPrefix(r.URL.Path, "/user/repos"):
			fmt.Fprint(w, `[
				{"full_name":"x/a","owner":{"login":"x"},"name":"a","pushed_at":"2026-05-18T10:00:00Z"},
				{"full_name":"x/b","owner":{"login":"x"},"name":"b","pushed_at":"2026-05-18T11:00:00Z"},
				{"full_name":"x/c","owner":{"login":"x"},"name":"c","pushed_at":"2026-05-18T12:00:00Z"}
			]`)
		case strings.HasPrefix(r.URL.Path, "/search/issues"):
			fmt.Fprint(w, `{"items":[]}`)
		case strings.HasPrefix(r.URL.Path, "/users/u/events"):
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	repos, err := Discover(c, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos (maxRepos cap), got %d", len(repos))
	}
	if repos[0].FullName != "x/c" || repos[1].FullName != "x/b" {
		t.Errorf("cap should keep most recently active: got %+v", repos)
	}
}
