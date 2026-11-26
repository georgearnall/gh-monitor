package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/georgearnall/gh-monitor/internal/state"
)

// scanStewarded returns the PR URLs that running claude processes were
// started to /steward. Always empty on Windows, where there's no cheap ps.
func scanStewarded() map[string]bool {
	if runtime.GOOS == "windows" {
		return nil
	}
	out, err := exec.Command("ps", "-axo", "args").Output()
	if err != nil {
		return nil
	}
	return parseStewarded(string(out))
}

// parseStewarded extracts the URL following "/steward " from each claude
// process line of `ps -axo args` output.
func parseStewarded(out string) map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if !isClaudeProcess(fields) {
			continue
		}
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] == "/steward" && strings.HasPrefix(fields[i+1], "https://") {
				set[fields[i+1]] = true
			}
		}
	}
	return set
}

// stewardRescanDelays are the cumulative waits after a launch at which the
// process list is rescanned for the new steward session.
var stewardRescanDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

// rescanStewarded waits until each cumulative delay, scans and stores the
// stewarded set, and stops early once url shows up.
func rescanStewarded(url string, delays []time.Duration, sleep func(time.Duration), scan func() map[string]bool, store func(map[string]bool)) {
	var waited time.Duration
	for _, d := range delays {
		sleep(d - waited)
		waited = d
		set := scan()
		store(set)
		if set[url] {
			return
		}
	}
}

// isClaudeProcess reports whether argv is a claude run, either the claude
// binary itself or claude's script run under node.
func isClaudeProcess(fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	switch filepath.Base(fields[0]) {
	case "claude":
		return true
	case "node":
		return len(fields) > 1 && (filepath.Base(fields[1]) == "claude" ||
			strings.HasSuffix(fields[1], "/@anthropic-ai/claude-code/cli.js"))
	}
	return false
}

// stewardLaunch decides what `s` does on the focused row. It returns the
// directory and argv for a new /steward session, or a footer message (and
// nil argv) explaining why nothing can be launched. stewarded is the set of
// PR URLs that already have a running steward session.
func stewardLaunch(st *state.State, f focusTarget, sourceDir string, stewarded map[string]bool, claudeOK bool) (string, []string, string) {
	if f.Panel != "prs" {
		return "", nil, "steward: pick one of your own PRs"
	}
	for _, p := range st.LastPRs {
		if prKey(p) != f.ID {
			continue
		}
		key := prKey(p)
		if !claudeOK {
			return "", nil, "claude not installed"
		}
		if stewarded[p.URL] {
			return "", nil, fmt.Sprintf("already stewarded: %s (close its claude tab to restart)", key)
		}
		repoPath, ok := resolveRepoPath(sourceDir, p.Repo)
		if !ok {
			return "", nil, fmt.Sprintf("repo not found in %s: %s", sourceDir, p.Repo)
		}
		// resolveWorktreePath also matches the main clone when its current
		// branch is the head branch.
		dir, ok := resolveWorktreePath(repoPath, p.HeadBranch)
		if !ok {
			return "", nil, "branch not checked out locally: " + p.HeadBranch
		}
		return dir, []string{"claude", "--permission-mode", "auto", "-n", "steward " + key, "/steward " + p.URL}, ""
	}
	return "", nil, "steward: pick one of your own PRs"
}
