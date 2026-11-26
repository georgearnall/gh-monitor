package main

import (
	"fmt"

	"github.com/georgearnall/gh-monitor/internal/state"
)

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
