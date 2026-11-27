package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/georgearnall/gh-monitor/internal/prs"
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

// stewardScanResult is a steward process scan and when it started.
type stewardScanResult struct {
	urls map[string]bool
	at   time.Time
}

// rescanStewarded waits until each cumulative delay, scans and stores the
// stewarded set with the scan's start time, and stops early once url shows up.
func rescanStewarded(url string, delays []time.Duration, sleep func(time.Duration), scan func() map[string]bool, store func(map[string]bool, time.Time)) {
	var waited time.Duration
	for _, d := range delays {
		sleep(d - waited)
		waited = d
		at := time.Now()
		set := scan()
		store(set, at)
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

// stewardPendingTTL is how long a launched PR stays marked as stewarded
// without a process scan confirming its session.
const stewardPendingTTL = 15 * time.Second

// mergeStewarded returns the stewarded set (scan plus pending launches) and
// the launches still pending: those the scan doesn't show yet and that are
// younger than stewardPendingTTL. pending is not modified.
func mergeStewarded(scan map[string]bool, pending map[string]time.Time, now time.Time) (map[string]bool, map[string]time.Time) {
	set := make(map[string]bool, len(scan)+len(pending))
	for url := range scan {
		set[url] = true
	}
	left := map[string]time.Time{}
	for url, at := range pending {
		if !scan[url] && now.Sub(at) < stewardPendingTTL {
			left[url] = at
			set[url] = true
		}
	}
	return set, left
}

// ownPR returns the focused row's PR when it is one of the user's own PRs.
func ownPR(st *state.State, focused focusTarget) (prs.PR, bool) {
	if focused.Panel == "prs" {
		for _, p := range st.LastPRs {
			if prKey(p) == focused.ID {
				return p, true
			}
		}
	}
	return prs.PR{}, false
}

// stewardRefusal returns the footer message explaining why `s` can't launch
// on p, or "" when only the checkout lookup remains. p is the zero PR when
// the focused row isn't one of the user's own PRs. stewarded is the set of
// PR URLs that already have a running steward session.
func stewardRefusal(p prs.PR, stewarded map[string]bool, claudeInstalled bool) string {
	switch {
	case p.URL == "":
		return "steward: pick one of your own PRs"
	case !claudeInstalled:
		return "claude not installed"
	case stewarded[p.URL]:
		return fmt.Sprintf("already stewarded: %s (close its claude tab to restart)", prKey(p))
	}
	return ""
}

// stewardDir returns the local checkout of p's head branch under
// sourceDir, or a footer message explaining why there isn't one.
func stewardDir(sourceDir string, p prs.PR) (dir, msg string) {
	repoPath, ok := resolveRepoPath(sourceDir, p.Repo)
	if !ok {
		return "", fmt.Sprintf("repo not found in %s: %s", sourceDir, p.Repo)
	}
	// resolveWorktreePath also matches the main clone when its current
	// branch is the head branch.
	dir, ok = resolveWorktreePath(repoPath, p.HeadBranch)
	if !ok {
		return "", "branch not checked out locally: " + p.HeadBranch
	}
	return dir, ""
}

// stewardCommand is the argv that starts a /steward session on p.
func stewardCommand(p prs.PR) []string {
	return []string{"claude", "--permission-mode", "auto", "-n", "steward " + prKey(p), "/steward " + p.URL}
}
