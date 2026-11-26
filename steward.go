package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

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
		if len(fields) == 0 || filepath.Base(fields[0]) != "claude" {
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
