package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
	"github.com/georgearnall/gha-monitor/internal/prs"
	"github.com/georgearnall/gha-monitor/internal/runs"
)

// isolate redirects statePath() to a per-test tempdir.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "gha-monitor", "state.json")
}

func TestLoad_Missing(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s == nil || s.Runs == nil {
		t.Fatalf("expected empty State with non-nil Runs, got %+v", s)
	}
	if len(s.Runs) != 0 {
		t.Errorf("expected zero runs, got %d", len(s.Runs))
	}
}

func TestLoad_Corrupt(t *testing.T) {
	path := isolate(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Errorf("expected error on corrupt state, got nil")
	}
}

func TestSaveLoad_Roundtrip(t *testing.T) {
	isolate(t)
	now := time.Now().UTC().Truncate(time.Second)

	original, err := Load()
	if err != nil {
		t.Fatalf("initial Load: %v", err)
	}
	original.Runs[42] = RunRecord{Status: "completed", Conclusion: "success", UpdatedAt: now}
	original.LastView = []runs.Run{
		{ID: 42, Repo: "x/y", WorkflowName: "CI", Branch: "main", Status: "completed", Conclusion: "success", URL: "https://github.com/x/y/runs/42", CreatedAt: now, UpdatedAt: now},
	}
	original.Repos = []discovery.Repo{{FullName: "x/y", Owner: "x", Name: "y", Activity: now, HTMLURL: "https://github.com/x/y"}}
	original.LastPRs = []prs.PR{{Repo: "x/y", Number: 1, Title: "test", URL: "https://github.com/x/y/pull/1", State: "SUCCESS", Passing: 3, Total: 3, UpdatedAt: now}}
	original.LastPoll = now
	original.LastRateLimit = ghclient.RateLimit{Limit: 5000, Remaining: 4823, ResetAt: now.Add(time.Hour)}
	original.EtagCache = map[string]ghclient.EtagEntry{
		"repos/x/y/actions/runs?per_page=10": {ETag: `"abc123"`, Body: []byte(`{"workflow_runs":[]}`)},
		"user/repos?sort=pushed":              {ETag: `"def456"`, Body: []byte(`[]`)},
	}

	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !reflect.DeepEqual(reloaded.Runs, original.Runs) {
		t.Errorf("Runs differ:\n  got %+v\n  want %+v", reloaded.Runs, original.Runs)
	}
	if !reflect.DeepEqual(reloaded.LastView, original.LastView) {
		t.Errorf("LastView differs:\n  got %+v\n  want %+v", reloaded.LastView, original.LastView)
	}
	if !reflect.DeepEqual(reloaded.Repos, original.Repos) {
		t.Errorf("Repos differ:\n  got %+v\n  want %+v", reloaded.Repos, original.Repos)
	}
	if !reflect.DeepEqual(reloaded.LastPRs, original.LastPRs) {
		t.Errorf("LastPRs differ:\n  got %+v\n  want %+v", reloaded.LastPRs, original.LastPRs)
	}
	if !reloaded.LastPoll.Equal(original.LastPoll) {
		t.Errorf("LastPoll differs: got %v, want %v", reloaded.LastPoll, original.LastPoll)
	}
	if !reflect.DeepEqual(reloaded.LastRateLimit, original.LastRateLimit) {
		t.Errorf("LastRateLimit differs:\n  got %+v\n  want %+v", reloaded.LastRateLimit, original.LastRateLimit)
	}
	if !reflect.DeepEqual(reloaded.EtagCache, original.EtagCache) {
		t.Errorf("EtagCache differs:\n  got %+v\n  want %+v", reloaded.EtagCache, original.EtagCache)
	}
}

func TestSave_AtomicWrite(t *testing.T) {
	path := isolate(t)
	s, _ := Load()
	s.Runs[1] = RunRecord{Status: "completed", Conclusion: "success", UpdatedAt: time.Now()}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// No tempfile should remain after a successful save.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("leftover file in state dir: %s", e.Name())
		}
	}
}

func TestObserve_FirstSeen_NoTransition(t *testing.T) {
	isolate(t)
	s, _ := Load()
	r := runs.Run{ID: 1, Status: "completed", Conclusion: "failure"}
	if got := s.Observe(r); got != TransitionNone {
		t.Errorf("first observation got %v, want TransitionNone", got)
	}
	if _, ok := s.Runs[1]; !ok {
		t.Errorf("Observe should record the run")
	}
}

func TestObserve_ActiveToFailure_Fires(t *testing.T) {
	isolate(t)
	s, _ := Load()
	id := int64(1)
	s.Observe(runs.Run{ID: id, Status: "in_progress"})
	if got := s.Observe(runs.Run{ID: id, Status: "completed", Conclusion: "failure"}); got != TransitionFailure {
		t.Errorf("active→failure got %v, want TransitionFailure", got)
	}
}

func TestObserve_ActiveToTimedOut_Fires(t *testing.T) {
	isolate(t)
	s, _ := Load()
	id := int64(1)
	s.Observe(runs.Run{ID: id, Status: "in_progress"})
	if got := s.Observe(runs.Run{ID: id, Status: "completed", Conclusion: "timed_out"}); got != TransitionFailure {
		t.Errorf("active→timed_out got %v, want TransitionFailure", got)
	}
}

func TestObserve_FailureToFailure_NoRefire(t *testing.T) {
	isolate(t)
	s, _ := Load()
	id := int64(1)
	s.Observe(runs.Run{ID: id, Status: "completed", Conclusion: "failure"})
	if got := s.Observe(runs.Run{ID: id, Status: "completed", Conclusion: "failure"}); got != TransitionNone {
		t.Errorf("failure→failure got %v, want TransitionNone (dedup)", got)
	}
}

func TestObserve_ActiveToCancelled_NoFire(t *testing.T) {
	isolate(t)
	s, _ := Load()
	id := int64(1)
	s.Observe(runs.Run{ID: id, Status: "in_progress"})
	if got := s.Observe(runs.Run{ID: id, Status: "completed", Conclusion: "cancelled"}); got != TransitionNone {
		t.Errorf("active→cancelled got %v, want TransitionNone", got)
	}
}

func TestObserve_ActiveToSuccess_NoFire(t *testing.T) {
	isolate(t)
	s, _ := Load()
	id := int64(1)
	s.Observe(runs.Run{ID: id, Status: "in_progress"})
	if got := s.Observe(runs.Run{ID: id, Status: "completed", Conclusion: "success"}); got != TransitionNone {
		t.Errorf("active→success got %v, want TransitionNone", got)
	}
}

func TestPrune(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()
	s.Runs[1] = RunRecord{UpdatedAt: now.Add(-30 * 24 * time.Hour)} // old, unseen → drop
	s.Runs[2] = RunRecord{UpdatedAt: now.Add(-30 * 24 * time.Hour)} // old, seen → keep
	s.Runs[3] = RunRecord{UpdatedAt: now.Add(-1 * time.Hour)}       // recent, unseen → keep
	s.Runs[4] = RunRecord{UpdatedAt: now.Add(-1 * time.Hour)}       // recent, seen → keep

	seen := map[int64]bool{2: true, 4: true}
	s.Prune(seen, 7*24*time.Hour)

	want := map[int64]bool{2: true, 3: true, 4: true}
	if len(s.Runs) != len(want) {
		t.Fatalf("after Prune got %d runs, want %d: %+v", len(s.Runs), len(want), s.Runs)
	}
	for id := range want {
		if _, ok := s.Runs[id]; !ok {
			t.Errorf("expected id %d to be kept", id)
		}
	}
	if _, dropped := s.Runs[1]; dropped {
		t.Errorf("expected id 1 to be pruned")
	}
}
