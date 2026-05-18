package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

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
	Runs map[int64]RunRecord `json:"runs"`
	path string
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
