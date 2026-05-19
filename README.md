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
    ● in_progress  Build      acme/payments         main      2m   open ↗

polled 11:44:53 · 20 repos · 3 notifs · 2 PRs · rate limit 4823/5000 · next poll in 30s
[↑↓] move  [↵] open  [m] mark read  [M] mark all  [r] refresh  [q] quit
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
  read); `m` marks the focused notification read; `M` marks all visible
  unread notifications read.
- **Polls in parallel** with per-call ETag conditional GETs. Most cycles
  cost a handful of rate-limit units against the 5000/hr budget.
- **Desktop notification** on workflow failure via OSC 9 (Ghostty /
  iTerm2) or `osascript` (other macOS terminals).
- **Adaptive layout**. Narrow terminals shed the PR branch column and
  use shorter status labels; the repo cell trims the organisation part
  with a leading ellipsis and drops the org entirely when too tight,
  always preserving the repo name.
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

### Keybindings (watch mode)

| Key | Action |
|---|---|
| `↑` / `↓` | Move the cursor through the visible rows across all panels |
| `↵` (enter) | Open the focused row in the browser. For notifications, also mark read. |
| `m` | Mark the focused notification read |
| `M` | Mark every visible unread notification read |
| `r`, `R`, space | Trigger an immediate refresh |
| `q`, `Q`, Ctrl-C | Quit cleanly, restore terminal, save state |

## What gets shown

**Notifications panel.** Inbound mentions, review requests, replies on
threads you're in, and activity on PRs you authored or were assigned to.
Filtered to PR threads only (issues are dropped). Sorted unread-first
then most-recently-updated. Read items stay dimmed for 7 days then drop
off.

**Pull requests panel.** Every open, non-draft pull request you authored
across all of GitHub, updated within `--pr-since`, sorted failing-first
then most-recently-updated. Columns:

- `CHECKS` aggregate status check rollup (`✓ N/M`, `✗ N/M`, `◐ N/M`,
  `· none`).
- `REVIEW` review decision (`✓ approved`, `✗ changes`, `◐ reviewed`,
  `· blocked`, or blank when no decision and no reviews yet). A `+N`
  suffix shows comment count.
- `TITLE`, `REPO`, `#NUMBER`, `BRANCH`, `AGE`, clickable `LINK`.

On narrow terminals the BRANCH column drops first; the status text
("pass" / "fail" / "wait") drops next; the REPO cell shrinks last,
trimming the organisation before the name and finally dropping the org
entirely when it would still overflow.

**Workflow runs panel.** Capped at 10 rows, sorted active-first then
most-recently-updated:

- Active runs (queued / in_progress / waiting) regardless of who
  triggered them.
- Completed runs only if **you** triggered them and they finished within
  the last 24 hours.
- Bot runs are dropped entirely. Renovate, Dependabot, and Copilot
  review workflows are filtered by actor / branch prefix / workflow
  name.

## Discovery

The monitored repo set is the union of three GitHub queries, deduped and
sorted by most-recent activity:

1. `GET /user/repos?sort=pushed&affiliation=owner,collaborator`: repos you
   directly push to or are a named collaborator on.
2. `GET /search/issues?q=author:@me+is:pr+is:open`: repos where you have
   open PRs (catches forks and contributor repos).
3. `GET /users/<you>/events`: your activity stream (catches repos
   accessed only via team membership, e.g. deployment / infra repos).

Discovery re-runs every 5 minutes (configurable via `--repo-refresh`).

## State

Persisted to `$XDG_CONFIG_HOME/gha-monitor/state.json` (or
`~/.config/gha-monitor/state.json`). Stores:

- Last rendered tables (so warm starts paint instantly).
- ETag cache (so warm cold starts mostly hit 304s and consume ~2
  rate-limit units instead of ~170). Degenerate ETags such as GitHub's
  sentinel `W/""` from `/notifications` are not cached, so PATCHed
  notifications reflect on the next refresh.
- Run-ID dedup map (so failure notifications fire once per run, even
  across restarts).
- Discovered repo list + last poll timestamp + rate-limit headroom.

Safe to delete at any time; the tool repopulates on next launch.

## Rate-limit posture

Conservative by design:

- All cached endpoints use `If-None-Match`; GitHub's docs guarantee 304
  responses don't decrement the primary 5000/hr budget.
- Refresh polling is bounded to 8 concurrent in-flight requests (well
  under GitHub's documented secondary limit of 100).
- The adaptive poll interval doubles when `X-RateLimit-Remaining` drops
  below 500.
- 403 / 429 responses honour `Retry-After`.
- The footer always shows current `X-RateLimit-Remaining/Limit`.

## Building and testing

```sh
go build -o gh-monitor .
go test ./... -race
go test ./... -cover
```

## License

MIT.
