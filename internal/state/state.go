package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
	"github.com/georgearnall/gha-monitor/internal/notifs"
	"github.com/georgearnall/gha-monitor/internal/prs"
	"github.com/georgearnall/gha-monitor/internal/runs"
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
	LastNotifs      []notifs.Notification         `json:"last_notifs,omitempty"`
	DismissedNotifs map[string]DismissEntry       `json:"dismissed_notifs,omitempty"`
	Repos           []discovery.Repo              `json:"repos,omitempty"`
	LastPoll        time.Time                     `json:"last_poll,omitempty"`
	LastRateLimit   ghclient.RateLimit            `json:"last_rate_limit,omitempty"`
	ViewerLogin     string                        `json:"viewer_login,omitempty"`
	EtagCache       map[string]ghclient.EtagEntry `json:"etag_cache,omitempty"`
	path            string

	// BgErr is the most recent error from a background task (dismiss
	// queue, mark-read, etc.). Surfaced in the UI footer so the user can
	// see failures the alt-screen would otherwise swallow.
	// Runtime-only; not persisted.
	BgErr   string    `json:"-"`
	BgErrAt time.Time `json:"-"`
}

// DismissEntry remembers a notification the user dismissed locally so the
// poll loop can suppress the "bounce-back" that occurs when GitHub's
// notifications endpoint returns the just-dismissed thread for up to
// ~60s before its server-side cache invalidates.
//
// UpdatedAt is the notification's updated_at at the moment of dismissal.
// When a fresh poll yields a notification with the same ID whose
// updated_at is not strictly newer, we suppress it. New activity on the
// same thread (newer updated_at) bypasses the filter.
//
// DismissedAt is the local wall-clock time of the dismissal, used by
// PruneDismissed to forget entries that GitHub has clearly forgotten too.
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
	s := &State{Runs: map[int64]RunRecord{}, path: p}

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

// PruneDismissed drops dismissal entries whose DismissedAt is older than
// maxAge. By that point GitHub's server-side cache has invalidated and
// the API itself will stop returning the dismissed item, so we don't
// need a local record anymore.
func (s *State) PruneDismissed(maxAge time.Duration) {
	if s.DismissedNotifs == nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for id, e := range s.DismissedNotifs {
		if e.DismissedAt.Before(cutoff) {
			delete(s.DismissedNotifs, id)
		}
	}
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
		return filepath.Join(x, "gha-monitor", "state.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gha-monitor", "state.json"), nil
}
