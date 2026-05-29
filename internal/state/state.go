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
