# gh-monitor

A long-running terminal companion that watches your inbound GitHub
notifications, open pull-request check statuses, and recent workflow runs
across every repo you actively contribute to.

```
gh-monitor

NOTIFICATIONS
    REASON     TITLE                                                REPO                  #     AGE  LINK
▶   @ mention  [NB-1525] Bootstrap order-partnership-transaction…   acme/partnerships     #1    5m   open ↗
    ◐ review   [NB-1343] Add Statsig experimentation support        acme/partnerships     #242  2h   open ↗
    + comment  NB-1068 Add PlanYourTrip                             acme/checkout         #248  4d   open ↗

PULL REQUESTS
    CHECKS      REVIEW      TITLE                                       REPO              #    BRANCH                 AGE   LINK
    ✗ 1/1      · blocked   Ensure tests cover corporate onboarding…    acme/billing-api  #88  test-corporate         2h    open ↗
    ✓ 6/6     ✓ approved  Rename transaction orchestration module      acme/checkout     #234 refactor/tx-orch       30m   open ↗

WORKFLOW RUNS
    STATUS         WORKFLOW   REPO                  BRANCH    AGE  LINK
    ● in_progress  Deploy     acme/payments         main      2m   open ↗
    └ ▰▰▱▱▱ 3/8 stg: Deploy service / Deploy to stg › Configure AWS credentials +1
    ✗ failure      Build      acme/checkout         feat/tx   1h   open ↗
    └ failed at test › Run unit tests

polled 11:44:53 · 20 repos · 3 notifs · 2 PRs · rate limit 4823/5000 · next poll in 30s
[↑↓] move  [↵] open  [m] read  [d] dismiss  [x] mute repo  [t] ticket  [g] terminal  [r] refresh  [?] config  [q] quit
```

## What it does

- **Auto-discovers** the repos you actively work in: recently pushed-to,
  with open PRs by you, or surfaced in your activity feed (catches
  deployment / infra repos you only have team-membership access to).
- **Three live panels**: inbound notifications (mentions, review requests,
  replies, activity on your own PRs), open non-draft PRs with check
  rollup + review decision + comment count, and currently-running or
  recently-finished workflow runs.
- **Single cursor across all three panels.** Arrow keys move row-by-row;
  Enter opens the focused item in the browser (and marks notifications
  read); `m` marks the focused notification read; `d` dismisses; `x` mutes
  the focused row's repo.
- **Polls in parallel** with per-call ETag conditional GETs. Most cycles
  cost a handful of rate-limit units against the 5000/hr budget.
- **Desktop notification** when one of **your own** workflow runs fails
  (other people's and bot runs never alert), naming the job and step that
  failed, plus optional alerts for new
  GitHub notifications and comments on your PRs. Delivered via OSC 9
  (Ghostty / iTerm2) or `osascript` (other macOS terminals). Toggle each
  category in the config pane (`?`). Not supported on Windows (no-op
  there; see [WINDOWS.md](WINDOWS.md)).
- **Mute noisy repos** with `x` from any panel. A muted repo disappears
  from every panel, stops being polled, never alerts, and doesn't count
  towards `--max-repos`. Unmute from the config pane.
- **Adaptive layout**. Every panel fits on one line per row down to 70
  columns. Narrow terminals shed branch, number and age columns, use
  shorter status labels, shrink the link to `↗` and cut titles; the repo
  cell trims the organisation part with a leading ellipsis and drops the
  org entirely when too tight, always preserving the repo name.
- **Cache-first startup**. Paints the last-known table from disk in
  ~100ms, then refreshes in the background. Cold launches (empty cache)
  take ~3s.
- **Resize-aware**. SIGWINCH triggers a debounced redraw so the layout
  reflows when you change your terminal size.

## Install

### As a `gh` extension (recommended)

```sh
gh extension install georgearnall/gh-monitor
gh monitor
```

### From source

```sh
git clone https://github.com/georgearnall/gh-monitor.git
cd gh-monitor
go build -o gh-monitor .
./gh-monitor
```

Requires Go 1.21+ and the `gh` CLI authenticated (`gh auth login`).
The token needs the `notifications` scope to populate the notifications
panel; everything else uses default `gh` scopes.

### Windows

Developed and tested on macOS/Linux. It builds and runs on Windows,
but the interactive keybindings (arrow keys, `q`, `m`, `d`, `x`, `r`)
are currently unreliable there — see [WINDOWS.md](WINDOWS.md) for
why and what's involved in fixing it. Desktop notifications and the
`--sound` flag are unsupported on Windows and silently no-op.

## Usage

```
gh monitor [flags] [list-repos | watch | help]
```

`watch` (default) launches the live foreground TUI. `list-repos` prints
the discovered repo set and exits. `help` prints the full help page.

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--max-repos N` | 20 | Cap the monitored working set |
| `--interval D` | 20s | Poll interval while runs are active |
| `--idle-interval D` | 60s | Poll interval when nothing is running |
| `--repo-refresh D` | 5m | How often to re-run discovery |
| `--pr-since D` | 1440h (60d) | Hide PRs not updated within this window (use 0 to disable) |
| `--once` | off | Single poll cycle then exit (script-friendly) |
| `--exclude owner/repo` | none | Skip a noisy repo (repeatable) |
| `--no-notify` | off | Suppress desktop notifications |
| `--sound` | off | Also play a system sound on failure |
| `--repo-source-dir DIR` | none | Base directory containing local repo clones, used by the `g` (open repo in terminal) action |

### Keybindings (watch mode)

| Key | Action |
|---|---|
| `↑` / `↓` | Move the cursor through the visible rows across all panels |
| `↵` (enter) | Open the focused row in the browser. For notifications, also mark read. |
| `m` | Mark the focused notification read, or dim the focused workflow run |
| `d` | Dismiss the focused row. Notifications are marked done on GitHub and removed from the inbox; workflow runs are hidden locally |
| `x` | Mute the focused row's repo: hidden from every panel, no longer polled, no alerts. Unmute from the config pane |
| `t` | Open the Jira ticket referenced in the focused row's title. Prompts for the Jira base URL on first use |
| `g` | Open the focused row's repo in a terminal (Ghostty preferred, with fallbacks). Prompts for the repo source directory on first use. Shows a footer message if the repo isn't cloned locally |
| `r`, `R`, space | Trigger an immediate refresh |
| `?` | Open / close the config pane |
| `q`, `Q`, Ctrl-C | Quit cleanly, restore terminal, save state |

In an inline prompt (Jira URL, repo source directory), Enter confirms and
Esc cancels.

### Config pane (`?`)

Shows the effective settings, the alert toggles, and every repo: monitored
repos by recent activity, then all muted repos (including ones discovery no
longer returns).

| Key | Action |
|---|---|
| `↑` / `↓` | Move the cursor through the repo list |
| `x` | Mute / unmute the repo under the cursor |
| `1` | Toggle alerts for your failed builds |
| `2` | Toggle alerts for all GitHub notifications |
| `3` | Toggle alerts for comments on PRs you authored |
| `?` | Close the pane |

## What gets shown

**Notifications panel.** Inbound mentions, review requests, replies on
threads you're in, and activity on PRs you authored or were assigned to.
Filtered to PR threads only (issues are dropped). Sorted unread-first
then most-recently-updated. Read items stay dimmed for 7 days then drop
off.

On narrow terminals the REPO cell drops its organisation first; then the
link shrinks to `↗`, the `#` and AGE columns drop, and the title is cut
(to no less than 20 columns). The repo name is trimmed last.

**Pull requests panel.** Every open, non-draft pull request you authored
across all of GitHub, updated within `--pr-since`, sorted failing-first
then most-recently-updated. Columns:

- `CHECKS` aggregate status check rollup (`✓ N/M`, `✗ N/M`, `◐ N/M`,
  `· none`).
- `REVIEW` review decision (`✓ approved`, `✗ changes`, `◐ reviewed`,
  `· blocked`, or blank when no decision and no reviews yet). A `+N`
  suffix shows comment count.
- `TITLE` (up to 80 columns when there is room), `REPO`, `#NUMBER`,
  `BRANCH`, `AGE`, clickable `LINK`.

On narrow terminals the title is cut back to 55 columns first; the
BRANCH column drops next, then the status text ("pass" / "fail" /
"wait"). The REPO cell then drops its organisation, and only after that
does the review word ("approved" / "reviewed" / ...) go, then the `#` and
AGE columns, then the link shrinks to `↗`. The title is cut last (to no
less than 20 columns), and if it still doesn't fit the repo name is
trimmed.

**Workflow runs panel.** Capped at 10 rows, sorted most-recently-updated
first:

- Active runs (queued / in_progress / waiting) regardless of who
  triggered them.
- Completed runs only if **you** triggered them and they finished within
  the last 48 hours.
- Active runs, and runs that finished in the last 30 minutes, are shown
  at full brightness. Older finished runs are greyed out, keeping the
  colour of their ✓ / ✗.
- Bot runs are dropped entirely. Renovate, Dependabot, the Copilot coding
  agent, and Copilot review workflows are filtered by actor / branch
  prefix / workflow name.

Active runs get a progress line underneath: a bar, jobs finished / total
(skipped jobs count as finished) and the current `job › step`, with `+N`
when more jobs are running in parallel. Failed runs get
`failed at job › step` instead. The line uses the full terminal width and
is only cut short when it would overflow. Runs with a single job get no
progress line: their current step (or the step that failed) sits next to
the status instead, e.g. `● in_progress · Run tests`. It costs one jobs
request per active run on each poll; a finished run's jobs are fetched once
and cached.

On narrow terminals the WORKFLOW name shrinks first (to 30 columns); the
status word ("in_progress" / "success" / "failure") drops next, leaving
the glyph; then the inline step is cut, with BRANCH dropped first if the
step would otherwise be cut below 8 columns. The REPO cell then drops its
organisation; after that the link shrinks to `↗` and the WORKFLOW name is
cut further (to 20 columns), and the repo name is trimmed last.

Muted repos are hidden from all three panels.

## Discovery

The monitored repo set is the union of three GitHub queries, deduped and
sorted by most-recent activity:

1. `GET /user/repos?sort=pushed&affiliation=owner,collaborator`: repos you
   directly push to or are a named collaborator on.
2. `GET /search/issues?q=author:@me+is:pr+is:open`: repos where you have
   open PRs (catches forks and contributor repos).
3. `GET /users/<you>/events`: your activity stream (catches repos
   accessed only via team membership, e.g. deployment / infra repos).

Excluded (`--exclude`) and muted repos are removed before the
`--max-repos` cap is applied, so they don't take places. If discovery
fails, polling continues against the last good repo list.

Discovery re-runs every 5 minutes (configurable via `--repo-refresh`).

## State

Persisted to `$XDG_CONFIG_HOME/gh-monitor/state.json` (or
`~/.config/gh-monitor/state.json`). Stores:

- Last rendered tables (so warm starts paint instantly).
- ETag cache (so warm cold starts mostly hit 304s and consume ~2
  rate-limit units instead of ~170). Degenerate ETags such as GitHub's
  sentinel `W/""` from `/notifications` are not cached, so PATCHed
  notifications reflect on the next refresh. Entries unused for an hour
  (e.g. repos that dropped out of discovery or were muted) are pruned.
- Run-ID dedup map (so failure notifications fire once per run, even
  across restarts).
- Discovered repo list + last poll timestamp + rate-limit headroom.
- Muted repos, dismissed / read markers, alert toggles, Jira URL and repo
  source directory.

Safe to delete at any time; the tool repopulates on next launch (you
lose mutes and settings).

## Rate-limit posture

Conservative by design:

- All cached endpoints use `If-None-Match`; GitHub's docs guarantee 304
  responses don't decrement the primary 5000/hr budget.
- Refresh polling is bounded to 8 concurrent in-flight requests (well
  under GitHub's documented secondary limit of 100).
- The poll interval rises to at least 2 minutes when
  `X-RateLimit-Remaining` drops below 500.
- Rate-limit responses (429, or 403 with `Retry-After`,
  `X-RateLimit-Remaining: 0` or a rate-limit message) honour
  `Retry-After`. Other 403s, such as SAML/SSO enforcement or missing
  Actions access on one repo, only skip that repo.
- Below the same 500 threshold, or while a rate-limited jobs request's
  `Retry-After` runs, run progress stops fetching and shows its last
  known value. That `Retry-After` also delays the next poll.
- The footer always shows current `X-RateLimit-Remaining/Limit`.

## Known limitations

- If one repo's run poll fails (for any reason other than a rate limit),
  its runs drop out of the panel until it recovers, and after 60s its
  dismissed / read markers are forgotten, so those runs reappear.
- Background errors (mark-read failures, desktop alert errors, state save
  errors) are printed to stderr, which draws over the TUI instead of
  using the footer.
- Progress reads the first 100 jobs of a run. For bigger runs the total
  is still right, but the current or failed job may be missed, and the
  run is refetched every poll rather than cached.
- The progress total counts the jobs GitHub has created so far. Jobs
  gated behind `needs:` or reusable workflows may only be counted once
  they're queued, so the total can grow mid-run.
- Telling a lone Esc apart from an escape sequence relies on the terminal
  sending each sequence in one write; over a very slow link an arrow key
  can occasionally be read as Esc followed by text.

## Building and testing

```sh
go build -o gh-monitor .
go test ./... -race
go test ./... -cover
```

## License

MIT.
