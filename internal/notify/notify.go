package notify

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Failure delivers a desktop notification for a failed workflow run.
//
// In Ghostty (and other terminals that honour OSC 9) we send the notification
// via an escape sequence — no fork, no system permission prompt. Otherwise we
// fall back to `osascript` for macOS native notifications.
func Failure(repo, workflow, branch, url string) error {
	title := "GHA failed: " + repo
	body := fmt.Sprintf("%s on %s", workflow, branch)

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
func PlayAlert() {
	const sound = "/System/Library/Sounds/Sosumi.aiff"
	_ = exec.Command("afplay", sound).Start()
}

// appleString returns an AppleScript-safe double-quoted string literal.
func appleString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}
