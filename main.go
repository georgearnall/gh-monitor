package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/georgearnall/gh-monitor/internal/discovery"
	"github.com/georgearnall/gh-monitor/internal/ghclient"
)

type stringSet map[string]bool

func (s *stringSet) String() string {
	if s == nil || *s == nil {
		return ""
	}
	keys := make([]string, 0, len(*s))
	for k := range *s {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

func (s *stringSet) Set(v string) error {
	if *s == nil {
		*s = stringSet{}
	}
	(*s)[v] = true
	return nil
}

func main() {
	fs := flag.NewFlagSet("gh-monitor", flag.ExitOnError)
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
	fs.DurationVar(&cfg.prSince, "pr-since", 60*24*time.Hour, "hide pull requests not updated within this duration (0 disables)")
	fs.BoolVar(&cfg.once, "once", false, "run a single poll cycle and exit")
	fs.Var(&cfg.excluded, "exclude", "owner/repo to exclude from monitoring (repeatable)")
	fs.BoolVar(&cfg.noNotify, "no-notify", false, "suppress desktop notifications")
	fs.BoolVar(&cfg.sound, "sound", false, "also play an audible alert on failure")
	fs.Usage = func() { fmt.Fprint(os.Stderr, helpText) }

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !startsWithDash(args[0]) {
		cmd = args[0]
		args = args[1:]
	}
	if cmd == "help" || cmd == "--help" {
		fmt.Print(helpText)
		return
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
		runListRepos(client, cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(2)
	}
}

const helpText = `gh-monitor - watch GitHub Actions runs, PR checks, and notifications

USAGE
  gh-monitor [flags]              Start the live watch TUI (default)
  gh-monitor watch [flags]        Same as above, explicit
  gh-monitor list-repos [flags]   Print the discovered repo set and exit
  gh-monitor help                 Show this help

FLAGS
  --max-repos N         Cap the monitored repo set (default 20)
  --interval D          Poll interval while runs are active (default 20s)
  --idle-interval D     Poll interval when nothing is running (default 1m)
  --repo-refresh D      How often to re-run discovery (default 5m)
  --pr-since D          Hide PRs not updated within this duration (default
                        1440h = 60 days). Use 0 to disable the filter.
  --once                Single poll cycle then exit (no TUI, script-friendly)
  --exclude owner/repo  Skip a noisy repo. Repeatable.
  --no-notify           Suppress desktop notifications on workflow failure
  --sound               Also play a system sound on failure

KEYBINDINGS (watch mode)
  ↑ / ↓                 Move cursor across all three panels (notifications,
                        PRs, and workflow runs)
  ↵  (enter)            Open focused row in browser. If it's a notification,
                        also mark it read.
  m                     Mark focused notification read (no-op on PRs/runs)
  M                     Mark every visible unread notification read
  d                     Dismiss the focused row. For notifications: marks
                        done and removes from inbox. For workflow runs:
                        hides the run locally until it falls off the poll
                        window (useful for in-progress runs you don't
                        care about).
  r  /  R  /  space     Refresh now (don't wait for the next interval)
  q  /  Q  /  Ctrl-C    Quit cleanly, restore terminal, save state

PANELS
  NOTIFICATIONS  Inbound mentions, review requests, replies on threads
                 you're in, and activity on PRs you authored or were
                 assigned to. Unread first then most-recent. Read items
                 stay dimmed for 7 days then drop off.
  PULL REQUESTS  Your open non-draft PRs across all of GitHub, with check
                 rollup, review decision, and comment count. Failing
                 first, then pending, then most-recent.
  WORKFLOW RUNS  Up to 10 rows. Active runs always shown (any actor);
                 completed runs only if you triggered them in the last
                 24h. Renovate / Dependabot / Copilot runs are filtered.

DISCOVERY
  The monitored repo set is the union of three queries, deduped and
  sorted by most recent activity:
    1. /user/repos?affiliation=owner,collaborator
    2. /search/issues?q=author:@me+is:pr+is:open
    3. /users/<you>/events  (catches team-membership repos)

STATE
  Persisted to $XDG_CONFIG_HOME/gh-monitor/state.json (or
  ~/.config/gh-monitor/state.json). Holds the last rendered tables,
  an ETag cache for cheap 304 polling, and a run-ID dedup map so
  failure notifications fire once per run across restarts. Safe to
  delete at any time; repopulated on next launch.

RATE LIMITS
  Conditional GETs use If-None-Match; 304 responses do not count
  against the 5000/hr REST budget. Concurrent in-flight requests are
  bounded to 8. The poll interval doubles when remaining budget drops
  below 500. 403 / 429 responses honour Retry-After.

AUTHENTICATION
  Reuses your gh CLI auth (run 'gh auth login' if you haven't). The
  token needs the notifications scope to populate the NOTIFICATIONS
  panel; everything else uses default scopes.
`

func runListRepos(client *ghclient.Client, cfg watchConfig) {
	repos, err := discovery.Discover(client, cfg.maxRepos)
	if err != nil {
		fail("discover: %v", err)
	}
	repos = filterExcluded(repos, cfg.excluded)
	for _, r := range repos {
		fmt.Printf("%-50s %s\n", r.FullName, r.Activity.Format("2006-01-02 15:04"))
	}
}

func filterExcluded(repos []discovery.Repo, excl stringSet) []discovery.Repo {
	if len(excl) == 0 {
		return repos
	}
	out := repos[:0]
	for _, r := range repos {
		if excl[r.FullName] {
			continue
		}
		out = append(out, r)
	}
	return out
}

func startsWithDash(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
