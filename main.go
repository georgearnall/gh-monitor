package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/notify"
	"github.com/georgearnall/gha-monitor/internal/runs"
	"github.com/georgearnall/gha-monitor/internal/state"
	"github.com/georgearnall/gha-monitor/internal/ui"
)

func main() {
	fs := flag.NewFlagSet("gha-monitor", flag.ExitOnError)
	maxRepos := fs.Int("max-repos", 20, "maximum number of repos to monitor")
	interval := fs.Duration("interval", 30*time.Second, "poll interval")
	repoRefresh := fs.Duration("repo-refresh", 5*time.Minute, "repo list refresh interval")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gha-monitor [flags] [list-repos|watch]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !startsWithDash(args[0]) {
		cmd = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	client, err := api.DefaultRESTClient()
	if err != nil {
		fail("auth: %v", err)
	}

	switch cmd {
	case "", "watch":
		runWatch(client, *maxRepos, *interval, *repoRefresh)
	case "list-repos":
		runListRepos(client, *maxRepos)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n", cmd)
		os.Exit(2)
	}
}

func runListRepos(client *api.RESTClient, maxRepos int) {
	repos, err := discovery.Discover(client, maxRepos)
	if err != nil {
		fail("discover: %v", err)
	}
	for _, r := range repos {
		fmt.Printf("%-50s %s\n", r.FullName, r.Activity.Format("2006-01-02 15:04"))
	}
}

func runWatch(client *api.RESTClient, maxRepos int, interval, repoRefresh time.Duration) {
	st, err := state.Load()
	if err != nil {
		fail("load state: %v", err)
	}

	var (
		repos       []discovery.Repo
		lastRefresh time.Time
	)

	for {
		if time.Since(lastRefresh) > repoRefresh || repos == nil {
			discovered, err := discovery.Discover(client, maxRepos)
			if err != nil {
				fmt.Fprintf(os.Stderr, "discover: %v\n", err)
			} else {
				repos = discovered
				lastRefresh = time.Now()
			}
		}

		polled, err := runs.Poll(client, repos)
		if err != nil {
			fmt.Fprintf(os.Stderr, "poll: %v\n", err)
		}

		seen := make(map[int64]bool, len(polled))
		for _, r := range polled {
			seen[r.ID] = true
			if st.Observe(r) == state.TransitionFailure {
				if err := notify.Failure(r.Repo, r.WorkflowName, r.Branch, r.URL); err != nil {
					fmt.Fprintf(os.Stderr, "notify: %v\n", err)
				}
			}
		}
		st.Prune(seen, 7*24*time.Hour)
		if err := st.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "save state: %v\n", err)
		}

		ui.Render(ui.Snapshot{
			Runs:       polled,
			RepoCount:  len(repos),
			PolledAt:   time.Now(),
			NextPollIn: interval,
		})

		time.Sleep(interval)
	}
}

func startsWithDash(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
