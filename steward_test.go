package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
`
	got := parseStewarded(out)
	want := map[string]bool{
		"https://github.com/acme/repo/pull/1": true,
		"https://github.com/acme/repo/pull/2": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseStewarded = %v, want %v", got, want)
	}
}

func TestStewardLaunch(t *testing.T) {
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

	st := mkState(t)
	st.LastPRs = []prs.PR{
		{Repo: "acme/repo", Number: 1, HeadBranch: "feature-x", URL: "https://github.com/acme/repo/pull/1"},
		{Repo: "acme/repo", Number: 2, HeadBranch: "main", URL: "https://github.com/acme/repo/pull/2"},
		{Repo: "acme/repo", Number: 3, HeadBranch: "nowhere", URL: "https://github.com/acme/repo/pull/3"},
		{Repo: "acme/missing", Number: 4, HeadBranch: "x", URL: "https://github.com/acme/missing/pull/4"},
	}
	st.LastAssignedPRs = []prs.PR{{Repo: "acme/repo", Number: 9, HeadBranch: "main"}}
	st.LastNotifs = []notifs.Notification{{ID: "n1", Repo: "acme/repo"}}

	abs := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	notOwn := "steward: pick one of your own PRs"
	cases := []struct {
		name      string
		f         focusTarget
		stewarded map[string]bool
		claude    bool
		wantDir   string
		wantMsg   string
	}{
		{"notification row", focusTarget{"notifs", "n1"}, nil, true, "", notOwn},
		{"assigned pr", focusTarget{"prs", "acme/repo#9"}, nil, true, "", notOwn},
		{"claude missing", focusTarget{"prs", "acme/repo#1"}, nil, false, "", "claude not installed"},
		{"already stewarded", focusTarget{"prs", "acme/repo#1"}, map[string]bool{"https://github.com/acme/repo/pull/1": true}, true, "", "already stewarded: acme/repo#1 (close its claude tab to restart)"},
		{"repo missing", focusTarget{"prs", "acme/missing#4"}, nil, true, "", "repo not found in " + src + ": acme/missing"},
		{"branch in worktree", focusTarget{"prs", "acme/repo#1"}, nil, true, linked, ""},
		{"branch is clone's current", focusTarget{"prs", "acme/repo#2"}, nil, true, clone, ""},
		{"branch nowhere", focusTarget{"prs", "acme/repo#3"}, nil, true, "", "branch not checked out locally: nowhere"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, argv, msg := stewardLaunch(st, c.f, src, c.stewarded, c.claude)
			if msg != c.wantMsg {
				t.Errorf("msg = %q, want %q", msg, c.wantMsg)
			}
			if c.wantDir == "" {
				if argv != nil {
					t.Errorf("argv = %q, want nil", argv)
				}
				return
			}
			if abs(dir) != abs(c.wantDir) {
				t.Errorf("dir = %q, want %q", dir, c.wantDir)
			}
		})
	}

	_, argv, _ := stewardLaunch(st, focusTarget{"prs", "acme/repo#1"}, src, nil, true)
	want := []string{"claude", "--permission-mode", "auto", "-n", "steward acme/repo#1", "/steward https://github.com/acme/repo/pull/1"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}
