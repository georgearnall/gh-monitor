package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/georgearnall/gh-monitor/internal/discovery"
	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
)

// Transition describes the change observed for a run between polls.
type Transition int

const (
	// TransitionNone means the run hasn't changed in a notification-worthy way,
	// or this is the first time we've seen it.
	TransitionNone Transition = iota
	// TransitionFailure means a previously-active run reached a failure state.
	TransitionFailure
)

type RunRecord struct {
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type State struct {
	Runs            map[int64]RunRecord           `json:"runs"`
	LastView        []runs.Run                    `json:"last_view,omitempty"`
	LastPRs         []prs.PR                      `json:"last_prs,omitempty"`
	LastAssignedPRs []prs.PR                      `json:"last_assigned_prs,omitempty"`
	LastNotifs      []notifs.Notification         `json:"last_notifs,omitempty"`
	DismissedNotifs map[string]DismissEntry       `json:"dismissed_notifs,omitempty"`
	DismissedRuns   map[int64]time.Time           `json:"dismissed_runs,omitempty"`
	MarkedReadRuns  map[int64]time.Time           `json:"marked_read_runs,omitempty"`
	MutedRepos      map[string]bool               `json:"muted_repos,omitempty"`
	Repos           []discovery.Repo              `json:"repos,omitempty"`
	LastPoll        time.Time                     `json:"last_poll,omitempty"`
	LastRateLimit   ghclient.RateLimit            `json:"last_rate_limit,omitempty"`
	ViewerLogin     string                        `json:"viewer_login,omitempty"`
	EtagCache       map[string]ghclient.EtagEntry `json:"etag_cache,omitempty"`
	path            string

	// JiraURL is the base URL for the user's Jira instance, used to make
	// ticket references in titles clickable. Set via the inline prompt or
	// --jira-url flag. Persisted so it survives restarts.
	JiraURL string `json:"jira_url,omitempty"`

	// NotifyFailedBuilds toggles the desktop alert for a workflow run
	// transitioning active→failure. Pointer so Load can distinguish "never
	// set" (nil → migrate to true, preserving pre-toggle behavior for both
	// fresh installs and state.json files that predate this field) from an
	// explicit false the user chose in the settings pane. Use
	// FailedBuildAlertsEnabled/SetFailedBuildAlerts rather than touching
	// this field directly.
	NotifyFailedBuilds *bool `json:"notify_failed_builds,omitempty"`

	// NotifyAllGitHub toggles a desktop alert for any new item in the
	// GitHub Notifications feed (any allowedReasons entry). Off by
	// default; the zero value is already correct, no migration needed.
	NotifyAllGitHub bool `json:"notify_all_github,omitempty"`

	// NotifyOwnPRComments toggles a desktop alert specifically for a new
	// top-level conversation comment or inline review comment on a PR the
	// viewer authored. Off by default; zero value already correct.
	NotifyOwnPRComments bool `json:"notify_own_pr_comments,omitempty"`

	// AlertedNotifs dedups notification-triggered desktop alerts. Shared by
	// NotifyAllGitHub and NotifyOwnPRComments so a notification that
	// qualifies for both only ever alerts once. Same shape as DismissEntry
	// deliberately: UpdatedAt is the notification's updated_at at last
	// consideration (so a fresh comment on an existing thread — same ID,
	// bumped updated_at — is detected as new); AlertedAt is local
	// wall-clock time for the prune minAge guard.
	AlertedNotifs map[string]NotifAlertRecord `json:"alerted_notifs,omitempty"`

	// BgErr is the most recent error from a background task (dismiss
	// queue, mark-read, etc.). Surfaced in the UI footer so the user can
	// see failures the alt-screen would otherwise swallow.
	// Runtime-only; not persisted.
	BgErr   string    `json:"-"`
	BgErrAt time.Time `json:"-"`
}

// DismissEntry remembers a notification the user dismissed locally so the
// poll loop can suppress the "bounce-back" that occurs because GitHub's
// /notifications endpoint with ?all=true keeps returning done threads
// indefinitely (community bug #152852).
//
// UpdatedAt is the notification's updated_at at the moment of dismissal.
// When a fresh poll yields a notification with the same ID whose
// updated_at is not strictly newer, we suppress it. New activity on the
// same thread (newer updated_at) bypasses the filter.
//
// DismissedAt is the local wall-clock time of the dismissal, used by
// PruneDismissedAbsent as a minimum-age guard so a poll-just-after-dismiss
// can't accidentally prune the entry before GitHub starts echoing it.
type DismissEntry struct {
	UpdatedAt   time.Time `json:"updated_at"`
	DismissedAt time.Time `json:"dismissed_at"`
}

// NotifAlertRecord remembers the last state of a notification considered for
// a desktop alert (toggles NotifyAllGitHub / NotifyOwnPRComments), so a
// repeat poll of the same unread notification doesn't re-alert every cycle.
// See ObserveNotifAlert.
type NotifAlertRecord struct {
	UpdatedAt time.Time `json:"updated_at"`
	AlertedAt time.Time `json:"alerted_at"`
}

// Load reads state from disk; missing file returns an empty State.
func Load() (*State, error) {
	p, err := statePath()
	if err != nil {
		return nil, err
	}
	s := &State{Runs: map[int64]RunRecord{}, DismissedRuns: map[int64]time.Time{}, MarkedReadRuns: map[int64]time.Time{}, MutedRepos: map[string]bool{}, path: p}

	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.SetFailedBuildAlerts(true)
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.Runs == nil {
		s.Runs = map[int64]RunRecord{}
	}
	if s.DismissedRuns == nil {
		s.DismissedRuns = map[int64]time.Time{}
	}
	if s.MarkedReadRuns == nil {
		s.MarkedReadRuns = map[int64]time.Time{}
	}
	if s.NotifyFailedBuilds == nil {
		// Either a brand-new state.json or one saved before this field
		// existed. Either way, default to true: fresh installs should
		// alert on build failures, and existing users shouldn't lose the
		// alert they already had just by upgrading.
		s.SetFailedBuildAlerts(true)
	}
	return s, nil
}

// Save writes state atomically via tempfile + rename.
func (s *State) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Observe records a run and reports any notification-worthy transition.
//
// First time we see a run, we record its current state without firing a
// notification. This prevents a wall of alerts at startup for runs that
// already failed before the tool was launched.
func (s *State) Observe(r runs.Run) Transition {
	prev, known := s.Runs[r.ID]
	s.Runs[r.ID] = RunRecord{
		Status:     r.Status,
		Conclusion: r.Conclusion,
		UpdatedAt:  r.UpdatedAt,
	}
	if !known {
		return TransitionNone
	}
	wasActive := prev.Conclusion == "" || prev.Conclusion == "neutral"
	if wasActive && r.IsFailure() {
		return TransitionFailure
	}
	return TransitionNone
}

// FailedBuildAlertsEnabled reports whether desktop alerts fire for a workflow
// run transitioning to failure. Unset (nil, e.g. a state.json predating this
// field) is treated as enabled, matching the tool's original behavior.
func (s *State) FailedBuildAlertsEnabled() bool {
	return s.NotifyFailedBuilds == nil || *s.NotifyFailedBuilds
}

// SetFailedBuildAlerts sets the failed-build alert toggle explicitly.
func (s *State) SetFailedBuildAlerts(v bool) {
	s.NotifyFailedBuilds = &v
}

// ObserveNotifAlert records notification id's current updated_at in the
// AlertedNotifs ledger and reports whether this is alert-worthy.
//
// Unlike a workflow run (a fresh, never-reused ID per run, so "first sight"
// safely means "just started"), GitHub reuses one notification ID for an
// entire PR thread across every comment on it, only bumping updated_at each
// time. So "first sight of this ID" does not reliably mean "just happened" —
// it's exactly as likely to mean "this thread's very first comment, which
// just happened while we were already watching" as "an old thread that
// existed before this ID was ever recorded." Naively mirroring Observe's
// unknown-on-first-sight rule here would mean a PR's first-ever comment
// never alerts (only a *second* comment on the same thread would, once the
// ID is already known) — silently defeating the most common case.
//
// coldStart resolves the ambiguity: pass true only for the very first poll
// against an empty ledger (a fresh install, or the first poll after this
// notification type has never been observed at all), which is the one
// situation where every currently-present ID really could be pre-existing
// inbox noise. In that case, first sight is recorded silently with no alert,
// same rationale as Observe's startup-quiet behavior. On every later poll,
// once the ledger is warm, an unknown ID is a genuinely new thread and fires
// immediately; an already-known ID only fires again if updatedAt has moved
// forward since it was last recorded.
func (s *State) ObserveNotifAlert(id string, updatedAt time.Time, coldStart bool) bool {
	if s.AlertedNotifs == nil {
		s.AlertedNotifs = map[string]NotifAlertRecord{}
	}
	prev, known := s.AlertedNotifs[id]
	var fire bool
	switch {
	case known:
		fire = updatedAt.After(prev.UpdatedAt)
	case !coldStart:
		fire = true
	}
	s.AlertedNotifs[id] = NotifAlertRecord{UpdatedAt: updatedAt, AlertedAt: time.Now()}
	return fire
}

// PruneAlertedNotifsAbsent drops AlertedNotifs entries whose ID is no longer
// present in the latest poll, gated by minAge so we don't race a fresh alert
// against its first poll. Mirrors PruneDismissedAbsent.
func (s *State) PruneAlertedNotifsAbsent(present map[string]bool, minAge time.Duration) {
	if s.AlertedNotifs == nil {
		return
	}
	cutoff := time.Now().Add(-minAge)
	for id, rec := range s.AlertedNotifs {
		if present[id] {
			continue
		}
		if rec.AlertedAt.After(cutoff) {
			continue
		}
		delete(s.AlertedNotifs, id)
	}
}

// RecordDismiss notes that the user dismissed a notification with the given
// ID at the given updated_at. Used by IsDismissed to suppress GitHub's
// cached bounce-back of the same thread version.
func (s *State) RecordDismiss(id string, updatedAt time.Time) {
	if s.DismissedNotifs == nil {
		s.DismissedNotifs = make(map[string]DismissEntry)
	}
	s.DismissedNotifs[id] = DismissEntry{UpdatedAt: updatedAt, DismissedAt: time.Now()}
}

// IsDismissed reports whether the (id, updatedAt) pair has been dismissed.
// Returns true only when an entry exists AND updatedAt is not newer than
// the recorded UpdatedAt, so genuinely new activity on the same thread
// (a fresh updated_at) passes through.
func (s *State) IsDismissed(id string, updatedAt time.Time) bool {
	e, ok := s.DismissedNotifs[id]
	if !ok {
		return false
	}
	return !updatedAt.After(e.UpdatedAt)
}

// PruneDismissedAbsent drops dismissal entries whose ID is no longer in
// the latest poll response, gated by minAge so we don't race a dismiss
// against its first poll. Once GitHub stops echoing the thread, the local
// guard has nothing to do, so we remove it.
//
// Time-based pruning doesn't work here: GitHub's ?all=true listing keeps
// returning done threads indefinitely (community bug #152852), so there's
// no fixed window after which the guard becomes redundant.
func (s *State) PruneDismissedAbsent(present map[string]bool, minAge time.Duration) {
	if s.DismissedNotifs == nil {
		return
	}
	cutoff := time.Now().Add(-minAge)
	for id, e := range s.DismissedNotifs {
		if present[id] {
			continue
		}
		if e.DismissedAt.After(cutoff) {
			continue
		}
		delete(s.DismissedNotifs, id)
	}
}

// DismissRun records that the user dismissed a workflow run by ID.
func (s *State) DismissRun(id int64) {
	if s.DismissedRuns == nil {
		s.DismissedRuns = map[int64]time.Time{}
	}
	s.DismissedRuns[id] = time.Now()
}

// IsRunDismissed reports whether a run has been dismissed by the user.
func (s *State) IsRunDismissed(id int64) bool {
	_, ok := s.DismissedRuns[id]
	return ok
}

// PruneRunDismissals drops dismissal entries whose run ID is no longer
// present in the latest poll, gated by minAge to avoid racing a dismiss
// against its immediately-following poll.
func (s *State) PruneRunDismissals(present map[int64]bool, minAge time.Duration) {
	cutoff := time.Now().Add(-minAge)
	for id, dismissedAt := range s.DismissedRuns {
		if present[id] {
			continue
		}
		if dismissedAt.After(cutoff) {
			continue
		}
		delete(s.DismissedRuns, id)
	}
}

// MarkRunRead records that the user has marked a workflow run as read (dimmed).
func (s *State) MarkRunRead(id int64) {
	if s.MarkedReadRuns == nil {
		s.MarkedReadRuns = map[int64]time.Time{}
	}
	s.MarkedReadRuns[id] = time.Now()
}

// IsRunRead reports whether a run has been marked read by the user.
func (s *State) IsRunRead(id int64) bool {
	_, ok := s.MarkedReadRuns[id]
	return ok
}

// PruneReadRuns drops read-run entries whose run ID is no longer present in
// the latest poll, gated by minAge to avoid racing a mark-read against its
// immediately-following poll.
func (s *State) PruneReadRuns(present map[int64]bool, minAge time.Duration) {
	cutoff := time.Now().Add(-minAge)
	for id, markedAt := range s.MarkedReadRuns {
		if present[id] {
			continue
		}
		if markedAt.After(cutoff) {
			continue
		}
		delete(s.MarkedReadRuns, id)
	}
}

// MuteRepo records that the user wants to hide other actors' runs from repo.
func (s *State) MuteRepo(repo string) {
	if s.MutedRepos == nil {
		s.MutedRepos = map[string]bool{}
	}
	s.MutedRepos[repo] = true
}

// Prune removes records for runs that haven't been seen recently enough
// to stay relevant. Keeps the state file from growing unbounded.
func (s *State) Prune(seen map[int64]bool, olderThan time.Duration) {
	cutoff := time.Now().Add(-olderThan)
	for id, rec := range s.Runs {
		if seen[id] {
			continue
		}
		if rec.UpdatedAt.Before(cutoff) {
			delete(s.Runs, id)
		}
	}
}

func statePath() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "gh-monitor", "state.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gh-monitor", "state.json"), nil
}
