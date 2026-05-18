# gh-monitor

A long-running terminal companion that watches your GitHub Actions runs and
open pull-request check statuses across every repo you actively contribute to.

```
gha-monitor

PULL REQUESTS
CHECKS      REVIEW          REPO                                          #     TITLE                                     BRANCH                    AGE   LINK
✗ 1/1 fail  · needs review  trainline-private/VatCalculationService       #88   Ensure tests cover corporate onboarding…  test-corporateonboarding  157d  open ↗
✓ 6/6 pass  ✓ approved      trainline-private/spacelift-registry          #234  Rename order-partnership-transaction-or…  rename-opto               2h    open ↗
· none      ✓ approved      trainline-private/Trainline.PlanExecution     #35   [ECOM-9026] Convert Sql statements to u…  ECOM-9026-upsert          528d  open ↗

WORKFLOW RUNS
STATUS         REPO                                       WORKFLOW   BRANCH      AGE  LINK
● in_progress  trainline-private/OrderProcessingService   Build      main        2m   open ↗

polled 11:44:53 · 20 repos · 3 PRs · rate limit 4823/5000 · next poll in 30s
[r] refresh  [q] quit
```

## What it does

- **Auto-discovers** the repos you actively work in — recently pushed-to,
  with open PRs by you, or surfaced in your activity feed (catches deployment
  / infra repos you only have team-membership access to).
- **Polls workflow runs in parallel** with per-call ETag conditional GETs.
  Most cycles cost ≤5 rate-limit units against the 5000/hr budget.
- **Two live tables**: open non-draft PRs (with check rollup + review state +
  comment count) and currently-running or recently-finished workflow runs.
- **Notifies** via OSC 9 (Ghostty / iTerm2) or `osascript` (other macOS
  terminals) the moment a workflow you triggered fails.
- **Cache-first startup**: paints the last-known table from disk in ~100ms,
  then refreshes in the background. Cold launches (empty cache) take ~3s.
- **Manual refresh** with `r`, quit with `q`. SIGINT exits cleanly,
  restores your terminal, and saves state.

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

## Usage

```
gh monitor [flags] [list-repos | watch]
```

`watch` (default) launches the live foreground TUI. `list-repos` prints the
discovered repo set and exits — useful for sanity-checking discovery.

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--max-repos N` | 20 | Cap the monitored working set |
| `--interval D` | 20s | Poll interval while runs are active |
| `--idle-interval D` | 60s | Poll interval when nothing is running |
| `--repo-refresh D` | 5m | How often to re-run discovery |
| `--once` | off | Single poll cycle then exit (script-friendly) |
| `--exclude owner/repo` | — | Skip a noisy repo (repeatable) |
| `--no-notify` | off | Suppress desktop notifications |
| `--sound` | off | Also play a system sound on failure |

### Keybindings (watch mode)

| Key | Action |
|---|---|
| `r`, `R`, space | Trigger an immediate refresh |
| `q`, `Q`, Ctrl-C | Quit cleanly |

## What gets shown

**PR table** — every open, non-draft pull request you authored across all of
GitHub, sorted failing-first then most-recently-updated. Columns:
- `CHECKS` aggregate status check rollup (`✓ N/M pass`, `✗ N/M fail`, `◐ N/M wait`, `· none`)
- `REVIEW` review decision (`✓ approved`, `✗ changes`, `◐ reviewed`,
  `· needs review`) with a `+N` suffix for comment count
- `REPO`, `#NUMBER`, `TITLE`, `BRANCH`, `AGE`, clickable `LINK`

**Workflow runs table** — capped at 10 rows, sorted active-first then
most-recently-updated:
- Active runs (queued / in_progress / waiting) regardless of who triggered
- Completed runs only if **you** triggered them and they finished within
  the last 24 hours
- Bot runs are dropped entirely — Renovate, Dependabot, and Copilot review
  workflows are filtered by actor / branch prefix / workflow name

## Discovery

The monitored repo set is the union of three GitHub queries, deduped and
sorted by most-recent activity:

1. `GET /user/repos?sort=pushed&affiliation=owner,collaborator` — repos you
   directly push to or are a named collaborator on.
2. `GET /search/issues?q=author:@me+is:pr+is:open` — repos where you have
   open PRs (catches forks and contributor repos).
3. `GET /users/<you>/events` — your activity stream (catches repos accessed
   only via team membership, e.g. deployment/infra repos).

Discovery re-runs every 5 minutes (configurable via `--repo-refresh`).

## State

`~/.config/gha-monitor/state.json` (or `$XDG_CONFIG_HOME/gha-monitor/state.json`).
Stores:

- Last rendered table (so warm starts paint instantly)
- ETag cache (so warm cold starts mostly hit 304s and consume ~2 rate-limit
  units instead of ~170)
- Run-ID dedup map (so failure notifications fire once per run, even
  across restarts)
- Discovered repo list + last poll timestamp + rate-limit headroom

Safe to delete at any time — the tool repopulates on next launch.

## Rate-limit posture

Conservative by design:

- All cached endpoints use `If-None-Match`; GitHub's docs guarantee 304
  responses don't decrement the primary 5000/hr budget.
- Refresh polling is bounded to 8 concurrent in-flight requests (well under
  GitHub's documented secondary limit of 100).
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
