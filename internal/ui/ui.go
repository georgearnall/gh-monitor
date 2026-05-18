package ui

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/georgearnall/gha-monitor/internal/runs"
)

const (
	ansiClear  = "\x1b[H\x1b[2J"
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiGreen  = "\x1b[32m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
)

type Snapshot struct {
	Runs          []runs.Run
	RepoCount     int
	RateRemaining int
	RateLimit     int
	PolledAt      time.Time
	NextPollIn    time.Duration
}

// Render redraws the status table. Safe to call when stdout is not a tty;
// ANSI control codes are suppressed in that case.
func Render(snap Snapshot) {
	tty := isTTY(os.Stdout)

	rows := visibleRows(snap.Runs)
	active, failed := countByOutcome(rows)

	if tty {
		fmt.Print(ansiClear)
		setWindowTitle(fmt.Sprintf("gha-monitor · %d active · %d recent failures", active, failed))
	}

	header(snap, tty)
	if len(rows) == 0 {
		fmt.Println(dim("no active runs or recent failures", tty))
	} else {
		writeTable(rows, tty)
	}
	footer(snap, tty)
}

func header(snap Snapshot, tty bool) {
	title := "gha-monitor"
	if tty {
		title = ansiBold + title + ansiReset
	}
	fmt.Println(title)
}

func footer(snap Snapshot, tty bool) {
	parts := []string{
		"polled " + snap.PolledAt.Format("15:04:05"),
		fmt.Sprintf("%d repos", snap.RepoCount),
	}
	if snap.RateLimit > 0 {
		parts = append(parts, fmt.Sprintf("rate limit %d/%d", snap.RateRemaining, snap.RateLimit))
	}
	if snap.NextPollIn > 0 {
		parts = append(parts, fmt.Sprintf("next poll in %s", snap.NextPollIn.Round(time.Second)))
	}
	line := join(parts, " · ")
	fmt.Println()
	fmt.Println(dim(line, tty))
}

func writeTable(rows []runs.Run, tty bool) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, dim("STATUS\tREPO\tWORKFLOW\tBRANCH\tAGE\tLINK", tty))
	for _, r := range rows {
		link := "open"
		if tty {
			link = hyperlink(r.URL, "open ↗")
		} else {
			link = r.URL
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			statusCell(r, tty),
			r.Repo,
			truncate(r.WorkflowName, 30),
			truncate(r.Branch, 30),
			ageString(r),
			link,
		)
	}
	tw.Flush()
}

func visibleRows(rs []runs.Run) []runs.Run {
	var out []runs.Run
	for _, r := range rs {
		if r.IsActive() || (r.IsFailure() && time.Since(r.UpdatedAt) < time.Hour) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].IsActive(), out[j].IsActive()
		if ai != aj {
			return ai
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func countByOutcome(rs []runs.Run) (active, failed int) {
	for _, r := range rs {
		if r.IsActive() {
			active++
		}
		if r.IsFailure() {
			failed++
		}
	}
	return
}

func statusCell(r runs.Run, tty bool) string {
	if r.IsActive() {
		return color(ansiYellow, "●", tty) + " " + r.Status
	}
	switch r.Conclusion {
	case "success":
		return color(ansiGreen, "✓", tty) + " success"
	case "failure", "timed_out", "startup_failure":
		return color(ansiRed, "✗", tty) + " " + r.Conclusion
	case "cancelled", "skipped":
		return color(ansiDim, "·", tty) + " " + r.Conclusion
	}
	return r.Conclusion
}

func ageString(r runs.Run) string {
	t := r.UpdatedAt
	if r.IsActive() {
		t = r.CreatedAt
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

func hyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

func setWindowTitle(s string) {
	fmt.Fprintf(os.Stderr, "\x1b]2;%s\x07", s)
}

func color(code, s string, tty bool) string {
	if !tty {
		return s
	}
	return code + s + ansiReset
}

func dim(s string, tty bool) string {
	if !tty {
		return s
	}
	return ansiDim + s + ansiReset
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
