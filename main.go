package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
	"github.com/georgearnall/gha-monitor/internal/notify"
	"github.com/georgearnall/gha-monitor/internal/runs"
	"github.com/georgearnall/gha-monitor/internal/state"
	"github.com/georgearnall/gha-monitor/internal/ui"
)

type watchConfig struct {
	maxRepos    int
	repoRefresh time.Duration

	baseInterval   time.Duration // when no active runs
	activeInterval time.Duration // when ≥1 active run
	lowQuotaFloor  time.Duration // when remaining < lowQuotaThreshold
	lowQuotaLimit  int
}

func main() {
	fs := flag.NewFlagSet("gha-monitor", flag.ExitOnError)
	cfg := watchConfig{
		baseInterval:   60 * time.Second,
		activeInterval: 20 * time.Second,
		lowQuotaFloor:  2 * time.Minute,
		lowQuotaLimit:  500,
	}
	fs.IntVar(&cfg.maxRepos, "max-repos", 20, "maximum number of repos to monitor")
	fs.DurationVar(&cfg.activeInterval, "interval", cfg.activeInterval, "poll interval while runs are active")
	fs.DurationVar(&cfg.baseInterval, "idle-interval", cfg.baseInterval, "poll interval while no runs are active")
	fs.DurationVar(&cfg.repoRefresh, "repo-refresh", 5*time.Minute, "repo list refresh interval")
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

	client, err := ghclient.New()
	if err != nil {
		fail("auth: %v", err)
	}

	switch cmd {
	case "", "watch":
		runWatch(client, cfg)
	case "list-repos":
		runListRepos(client, cfg.maxRepos)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n", cmd)
		os.Exit(2)
	}
}

func runListRepos(client *ghclient.Client, maxRepos int) {
	repos, err := discovery.Discover(client, maxRepos)
	if err != nil {
		fail("discover: %v", err)
	}
	for _, r := range repos {
		fmt.Printf("%-50s %s\n", r.FullName, r.Activity.Format("2006-01-02 15:04"))
	}
}

func runWatch(client *ghclient.Client, cfg watchConfig) {
	st, err := state.Load()
	if err != nil {
		fail("load state: %v", err)
	}

	var (
		repos       []discovery.Repo
		lastRefresh time.Time
	)

	for {
		if time.Since(lastRefresh) > cfg.repoRefresh || repos == nil {
			discovered, err := discovery.Discover(client, cfg.maxRepos)
			if err != nil {
				fmt.Fprintf(os.Stderr, "discover: %v\n", err)
				if rl, ok := ghclient.AsRateLimited(err); ok {
					sleepWithFloor(rl.RetryAfter)
					continue
				}
			} else {
				repos = discovered
				lastRefresh = time.Now()
			}
		}

		polled, pollErr := runs.Poll(client, repos)
		if pollErr != nil {
			fmt.Fprintf(os.Stderr, "poll: %v\n", pollErr)
		}

		active := 0
		seen := make(map[int64]bool, len(polled))
		for _, r := range polled {
			seen[r.ID] = true
			if r.IsActive() {
				active++
			}
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

		rl := client.RateLimit()
		next := cfg.nextInterval(active, rl, pollErr)

		ui.Render(ui.Snapshot{
			Runs:          polled,
			RepoCount:     len(repos),
			RateRemaining: rl.Remaining,
			RateLimit:     rl.Limit,
			PolledAt:      time.Now(),
			NextPollIn:    next,
		})

		time.Sleep(next)
	}
}

func (c watchConfig) nextInterval(active int, rl ghclient.RateLimit, pollErr error) time.Duration {
	if rlErr, ok := ghclient.AsRateLimited(pollErr); ok {
		return max(rlErr.RetryAfter, c.lowQuotaFloor)
	}
	d := c.baseInterval
	if active > 0 {
		d = c.activeInterval
	}
	if rl.Limit > 0 && rl.Remaining < c.lowQuotaLimit {
		if c.lowQuotaFloor > d {
			d = c.lowQuotaFloor
		}
	}
	return d
}

func sleepWithFloor(d time.Duration) {
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	time.Sleep(d)
}

func startsWithDash(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
