package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/prs"
)

func TestParseStewarded(t *testing.T) {
	out := `ARGS
/sbin/launchd
claude --permission-mode auto -n steward acme/repo#1 /steward https://github.com/acme/repo/pull/1
/usr/local/bin/claude -n steward acme/repo#2 /steward https://github.com/acme/repo/pull/2
vim /steward https://github.com/acme/repo/pull/3
claude /steward stop
claude --resume https://github.com/acme/repo/pull/5
node /opt/bin/claude -n steward acme/repo#6 /steward https://github.com/acme/repo/pull/6
/usr/bin/node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js /steward https://github.com/acme/repo/pull/7
node /srv/app.js /steward https://github.com/acme/repo/pull/8
`
	got := parseStewarded(out)
	want := map[string]bool{
		"https://github.com/acme/repo/pull/1": true,
		"https://github.com/acme/repo/pull/2": true,
		"https://github.com/acme/repo/pull/6": true,
		"https://github.com/acme/repo/pull/7": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseStewarded = %v, want %v", got, want)
	}
}

func TestRescanStewarded_StopsOnceURLAppears(t *testing.T) {
	const url = "https://github.com/acme/repo/pull/1"
	var slept []time.Duration
	var stored []map[string]bool
	scans := 0
	rescanStewarded(url, []time.Duration{2, 5, 10},
		func(d time.Duration) { slept = append(slept, d) },
		func() map[string]bool {
			scans++
			if scans == 2 {
				return map[string]bool{url: true}
			}
			return map[string]bool{}
		},
		func(m map[string]bool) { stored = append(stored, m) })
	if want := []time.Duration{2, 3}; !reflect.DeepEqual(slept, want) {
		t.Errorf("slept = %v, want %v", slept, want)
	}
	if len(stored) != 2 || !stored[1][url] {
		t.Errorf("stored = %v, want two scans ending with %s", stored, url)
	}
}

func TestRescanStewarded_GivesUpAfterLastDelay(t *testing.T) {
	scans := 0
	rescanStewarded("u", []time.Duration{2, 5, 10}, func(time.Duration) {},
		func() map[string]bool { scans++; return nil }, func(map[string]bool) {})
	if scans != 3 {
		t.Errorf("scans = %d, want 3", scans)
	}
}

func TestOwnPR_OnlyMatchesOwnPRs(t *testing.T) {
	st := mkState(t)
	own := prs.PR{Repo: "acme/repo", Number: 1, URL: "https://github.com/acme/repo/pull/1"}
	st.LastPRs = []prs.PR{own}
	st.LastAssignedPRs = []prs.PR{{Repo: "acme/repo", Number: 9}}
	st.LastNotifs = []notifs.Notification{{ID: "n1", Repo: "acme/repo"}}
	if p, ok := ownPR(st, focusTarget{"prs", "acme/repo#1"}); !ok || p != own {
		t.Errorf("own PR: got %v, %v", p, ok)
	}
	for _, f := range []focusTarget{{"prs", "acme/repo#9"}, {"notifs", "n1"}, {}} {
		if _, ok := ownPR(st, f); ok {
			t.Errorf("ownPR(%v) ok, want not found", f)
		}
	}
}

func TestStewardRefusal(t *testing.T) {
	own := prs.PR{Repo: "acme/repo", Number: 1, URL: "https://github.com/acme/repo/pull/1"}
	stewarded := map[string]bool{own.URL: true}
	cases := []struct {
		name      string
		p         prs.PR
		stewarded map[string]bool
		claude    bool
		want      string
	}{
		{"own pr ok", own, nil, true, ""},
		{"not own pr", prs.PR{}, stewarded, false, "steward: pick one of your own PRs"},
		{"claude missing", own, stewarded, false, "claude not installed"},
		{"already stewarded", own, stewarded, true, "already stewarded: acme/repo#1 (close its claude tab to restart)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stewardRefusal(c.p, c.stewarded, c.claude); got != c.want {
				t.Errorf("stewardRefusal = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStewardDir(t *testing.T) {
	src := t.TempDir()
	clone := filepath.Join(src, "repo")
	mustMkdirAll(t, clone)
	gitOrSkip(t, clone, "init", "-q", "-b", "main")
	gitOrSkip(t, clone, "config", "user.email", "test@example.com")
	gitOrSkip(t, clone, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOrSkip(t, clone, "add", "f.txt")
	gitOrSkip(t, clone, "commit", "-q", "-m", "init")
	gitOrSkip(t, clone, "branch", "feature-x")
	gitOrSkip(t, clone, "branch", "nowhere")
	linked := filepath.Join(t.TempDir(), "linked")
	gitOrSkip(t, clone, "worktree", "add", "-q", linked, "feature-x")

	abs := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	cases := []struct {
		name    string
		p       prs.PR
		wantDir string
		wantMsg string
	}{
		{"repo missing", prs.PR{Repo: "acme/missing", HeadBranch: "x"}, "", "repo not found in " + src + ": acme/missing"},
		{"branch in worktree", prs.PR{Repo: "acme/repo", HeadBranch: "feature-x"}, linked, ""},
		{"branch is clone's current", prs.PR{Repo: "acme/repo", HeadBranch: "main"}, clone, ""},
		{"branch nowhere", prs.PR{Repo: "acme/repo", HeadBranch: "nowhere"}, "", "branch not checked out locally: nowhere"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, msg := stewardDir(src, c.p)
			if msg != c.wantMsg {
				t.Errorf("msg = %q, want %q", msg, c.wantMsg)
			}
			if c.wantDir == "" {
				if dir != "" {
					t.Errorf("dir = %q, want empty", dir)
				}
				return
			}
			if abs(dir) != abs(c.wantDir) {
				t.Errorf("dir = %q, want %q", dir, c.wantDir)
			}
		})
	}
}

func TestStewardCommand(t *testing.T) {
	got := stewardCommand(prs.PR{Repo: "acme/repo", Number: 1, URL: "https://github.com/acme/repo/pull/1"})
	want := []string{"claude", "--permission-mode", "auto", "-n", "steward acme/repo#1", "/steward https://github.com/acme/repo/pull/1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stewardCommand = %q, want %q", got, want)
	}
}
