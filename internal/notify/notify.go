package notify

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Failure delivers a desktop notification for a failed workflow run.
//
// In Ghostty (and other terminals that honour OSC 9) we send the notification
// via an escape sequence — no fork, no system permission prompt. Otherwise we
// fall back to `osascript` for macOS native notifications.
//
// Desktop notifications are not supported on Windows: there's no OSC 9
// terminal to fall back on and no equivalent of `osascript` to shell out to.
// Rather than fail noisily on every workflow failure, this is a no-op there.
// See WINDOWS.md.
func Failure(repo, workflow, branch, url string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	title := "GHA failed: " + repo
	body := fmt.Sprintf("%s on %s", workflow, branch)
	return deliver(title, body, url)
}

// Comment delivers a desktop notification for a new top-level conversation
// comment or inline code-review comment on a PR the viewer authored.
func Comment(repo string, prNumber int, prTitle, url string) error {
	title := fmt.Sprintf("New comment: %s#%d", repo, prNumber)
	return deliver(title, prTitle, url)
}

// NewNotification delivers a desktop notification for a new item in the
// GitHub Notifications feed.
func NewNotification(repo string, prNumber int, reason, title, url string) error {
	notifTitle := fmt.Sprintf("%s: %s#%d", reasonLabel(reason), repo, prNumber)
	return deliver(notifTitle, title, url)
}

// reasonLabel turns a GitHub notification reason into a short human label
// for the alert title.
func reasonLabel(reason string) string {
	switch reason {
	case "mention", "team_mention":
		return "Mentioned"
	case "review_requested":
		return "Review requested"
	case "assign":
		return "Assigned"
	case "comment":
		return "New comment"
	default:
		return "GitHub notification"
	}
}

// deliver sends a desktop notification with the given title/body, linking to
// url where the delivery mechanism supports it.
//
// In Ghostty (and other terminals that honour OSC 9) we send the notification
// via an escape sequence — no fork, no system permission prompt. Otherwise we
// fall back to `osascript` for macOS native notifications.
func deliver(title, body, url string) error {
	switch {
	case supportsOSC9():
		return sendOSC9(title, body)
	default:
		return sendOsascript(title, body, url)
	}
}

func supportsOSC9() bool {
	if !isTTY(os.Stderr) {
		return false
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "ghostty", "iTerm.app":
		return true
	}
	return false
}

func sendOSC9(title, body string) error {
	msg := strings.ReplaceAll(title+" — "+body, "\x07", " ")
	_, err := fmt.Fprintf(os.Stderr, "\x1b]9;%s\x07", msg)
	return err
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func sendOsascript(title, body, url string) error {
	subtitle := body
	if url != "" {
		body = url
	}
	script := fmt.Sprintf(
		`display notification %s with title %s subtitle %s`,
		appleString(body), appleString(title), appleString(subtitle),
	)
	return exec.Command("osascript", "-e", script).Run()
}

// PlayAlert plays a short system sound. Best-effort, errors are ignored.
// No-op on Windows: see the comment on Failure above.
func PlayAlert() {
	if runtime.GOOS == "windows" {
		return
	}
	const sound = "/System/Library/Sounds/Sosumi.aiff"
	_ = exec.Command("afplay", sound).Start()
}

// appleString returns an AppleScript-safe double-quoted string literal.
func appleString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}
