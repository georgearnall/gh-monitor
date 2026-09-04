package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
	"github.com/georgearnall/gh-monitor/internal/state"
	"github.com/georgearnall/gh-monitor/internal/ui"
)

// focusTarget identifies one focused row across any of the three panels.
// Zero value means "no focus" (empty state).
type focusTarget struct {
	Panel string // "notifs" | "prs" | "runs"
	ID    string // panel-specific identifier
}

// prKey returns the stable identifier we use to track a PR row across
// refreshes: "owner/repo#number".
func prKey(p prs.PR) string {
	return fmt.Sprintf("%s#%d", p.Repo, p.Number)
}

// runKey returns the stable identifier for a workflow run row.
func runKey(r runs.Run) string {
	return strconv.FormatInt(r.ID, 10)
}

// cursorTargets materialises the flat ordered list of every focusable row
// across all three panels, in the same order they're rendered. Critically,
// it applies the same VisibleNotifs / VisibleRows filters the UI uses so
// the cursor never lands on a row that isn't on screen (otherwise arrow
// keys feel "sticky" as they walk through invisible items).
func cursorTargets(st *state.State) []focusTarget {
	notifRows := ui.VisibleNotifs(st.LastNotifs)
	runRows := ui.VisibleRows(st.LastView, st.ViewerLogin)
	out := make([]focusTarget, 0, len(notifRows)+len(st.LastPRs)+len(st.LastAssignedPRs)+len(runRows))
	for _, n := range notifRows {
		out = append(out, focusTarget{"notifs", n.ID})
	}
	for _, p := range st.LastPRs {
		out = append(out, focusTarget{"prs", prKey(p)})
	}
	for _, p := range st.LastAssignedPRs {
		out = append(out, focusTarget{"prs", prKey(p)})
	}
	for _, r := range runRows {
		out = append(out, focusTarget{"runs", runKey(r)})
	}
	return out
}

// pickFocus returns current if it still appears in the cursor target list,
// otherwise falls back to the first unread notification (if visible),
// otherwise the first target row overall. Returns the zero value when
// there's nothing focusable.
func pickFocus(st *state.State, current focusTarget) focusTarget {
	targets := cursorTargets(st)
	if current.Panel != "" {
		for _, t := range targets {
			if t == current {
				return current
			}
		}
	}
	for _, n := range ui.VisibleNotifs(st.LastNotifs) {
		if n.Unread {
			return focusTarget{"notifs", n.ID}
		}
	}
	if len(targets) > 0 {
		return targets[0]
	}
	return focusTarget{}
}

// moveFocus advances the cursor by delta rows across the flat target list,
// clamped at both ends.
func moveFocus(st *state.State, current focusTarget, delta int) focusTarget {
	targets := cursorTargets(st)
	if len(targets) == 0 {
		return focusTarget{}
	}
	idx := -1
	for i, t := range targets {
		if t == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		return targets[0]
	}
	next := idx + delta
	if next < 0 {
		next = 0
	}
	if next >= len(targets) {
		next = len(targets) - 1
	}
	return targets[next]
}

// applyRunDismiss removes the run with the given string ID from LastView and
// returns the focus that should take its place. Mirrors applyDismiss but
// operates on the runs panel. The bool reports whether the run was present.
// Pure: no I/O, no goroutines.
func applyRunDismiss(st *state.State, id string) (focusTarget, bool) {
	runID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return focusTarget{}, false
	}
	// Determine current visible position for next-focus calculation.
	visibleBefore := ui.VisibleRows(st.LastView, st.ViewerLogin)
	visIdx := -1
	for i, r := range visibleBefore {
		if r.ID == runID {
			visIdx = i
			break
		}
	}
	if visIdx < 0 {
		return focusTarget{}, false
	}
	// Remove from raw LastView.
	for i, r := range st.LastView {
		if r.ID == runID {
			st.LastView = append(st.LastView[:i], st.LastView[i+1:]...)
			break
		}
	}
	visibleAfter := ui.VisibleRows(st.LastView, st.ViewerLogin)
	if len(visibleAfter) == 0 {
		return pickFocus(st, focusTarget{}), true
	}
	ni := visIdx
	if ni >= len(visibleAfter) {
		ni = len(visibleAfter) - 1
	}
	return focusTarget{"runs", runKey(visibleAfter[ni])}, true
}

// applyDismiss removes the notification with id from local state and
// returns the focus that should take its place: the next row in the
// notifications list (clamped at the end), or pickFocus's fallback when
// the notifications panel becomes empty. The bool reports whether the
// notification was actually present. Pure: no I/O, no goroutines.
func applyDismiss(st *state.State, id string) (focusTarget, bool) {
	idx := -1
	for i, n := range st.LastNotifs {
		if n.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return focusTarget{}, false
	}
	st.LastNotifs = append(st.LastNotifs[:idx], st.LastNotifs[idx+1:]...)
	if len(st.LastNotifs) == 0 {
		return pickFocus(st, focusTarget{}), true
	}
	ni := idx
	if ni >= len(st.LastNotifs) {
		ni = len(st.LastNotifs) - 1
	}
	return focusTarget{"notifs", st.LastNotifs[ni].ID}, true
}

// openFocused launches the URL of whatever row the cursor is on in the
// user's default browser. Non-blocking.
func openFocused(st *state.State, f focusTarget) {
	url := focusedURL(st, f)
	if url == "" {
		return
	}
	openURL(url)
}

// openURL launches url in the default browser. Non-blocking.
func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// `start` is a cmd.exe builtin, not a binary, so it must be run via
		// cmd /c. The empty title argument is required: without it, `start`
		// treats a URL containing `&` as its (quoted) window title instead
		// of the target to open.
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
	}
}

func focusedURL(st *state.State, f focusTarget) string {
	switch f.Panel {
	case "notifs":
		for _, n := range st.LastNotifs {
			if n.ID == f.ID {
				return n.URL
			}
		}
	case "prs":
		for _, p := range st.LastPRs {
			if prKey(p) == f.ID {
				return p.URL
			}
		}
		for _, p := range st.LastAssignedPRs {
			if prKey(p) == f.ID {
				return p.URL
			}
		}
	case "runs":
		for _, r := range st.LastView {
			if runKey(r) == f.ID {
				return r.URL
			}
		}
	}
	return ""
}

// focusedRepo returns the "owner/name" of whatever row the cursor is on, or
// "" if nothing is focused or the row's repo can't be determined.
func focusedRepo(st *state.State, f focusTarget) string {
	switch f.Panel {
	case "notifs":
		for _, n := range st.LastNotifs {
			if n.ID == f.ID {
				return n.Repo
			}
		}
	case "prs":
		for _, p := range st.LastPRs {
			if prKey(p) == f.ID {
				return p.Repo
			}
		}
		for _, p := range st.LastAssignedPRs {
			if prKey(p) == f.ID {
				return p.Repo
			}
		}
	case "runs":
		for _, r := range st.LastView {
			if runKey(r) == f.ID {
				return r.Repo
			}
		}
	}
	return ""
}

// resolveRepoPath finds a local clone of "owner/name" under sourceDir.
// Tries the flat layout (sourceDir/name) first, then the owner/name
// nested layout. A candidate only counts if it has a .git entry, so an
// unrelated same-named directory isn't mistaken for the repo. sourceDir may
// start with "~" (expanded against the user's home directory); this is
// typed into the app's own inline prompt, not a shell, so it never gets
// tilde-expanded for us.
func resolveRepoPath(sourceDir, fullName string) (string, bool) {
	sourceDir = expandHome(sourceDir)
	if sourceDir == "" || fullName == "" {
		return "", false
	}
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || name == "" {
		return "", false
	}
	for _, candidate := range []string{
		filepath.Join(sourceDir, name),
		filepath.Join(sourceDir, owner, name),
	} {
		if info, err := os.Stat(filepath.Join(candidate, ".git")); err == nil && info != nil {
			return candidate, true
		}
	}
	return "", false
}

// expandHome resolves a leading "~" or "~/..." against the user's home
// directory. Paths not starting with "~" are returned unchanged. Any
// failure to determine the home directory (or a bare "~user" form we don't
// support) leaves the path untouched, so the eventual .git check just fails
// cleanly instead of erroring out here.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// openRepoInTerminal launches a terminal in path. Prefers Ghostty on macOS
// (opening a new tab if it's already running), falling back to iTerm and
// then Terminal.app; prefers Windows Terminal on Windows, falling back to
// cmd.exe; tries common Linux terminal emulators in turn. Best-effort: logs
// to stderr rather than erroring if nothing usable is found.
func openRepoInTerminal(path string) {
	switch runtime.GOOS {
	case "darwin":
		for _, app := range []string{"Ghostty", "iTerm", "Terminal"} {
			if exec.Command("open", "-a", app, path).Run() == nil {
				return
			}
		}
		fmt.Fprintf(os.Stderr, "open terminal: no known terminal app found (tried Ghostty, iTerm, Terminal)\n")
	case "windows":
		if _, err := exec.LookPath("wt"); err == nil {
			if err := exec.Command("wt", "-d", path).Start(); err == nil {
				return
			}
		}
		if err := exec.Command("cmd", "/c", "start", "", "cmd", "/K", "cd /d "+path).Start(); err != nil {
			fmt.Fprintf(os.Stderr, "open terminal: %v\n", err)
		}
	default:
		terms := []struct {
			bin  string
			args []string
		}{
			{"gnome-terminal", []string{"--working-directory=" + path}},
			{"konsole", []string{"--workdir", path}},
			{"xterm", []string{"-e", "cd " + path + " && exec $SHELL"}},
		}
		for _, t := range terms {
			if _, err := exec.LookPath(t.bin); err != nil {
				continue
			}
			if err := exec.Command(t.bin, t.args...).Start(); err == nil {
				return
			}
		}
		fmt.Fprintf(os.Stderr, "open terminal: no known terminal emulator found on PATH\n")
	}
}
