package discovery

import (
	"sort"
	"testing"
	"time"
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
