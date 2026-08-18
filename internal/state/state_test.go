package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/georgearnall/gh-monitor/internal/discovery"
	"github.com/georgearnall/gh-monitor/internal/ghclient"
	"github.com/georgearnall/gh-monitor/internal/notifs"
	"github.com/georgearnall/gh-monitor/internal/prs"
	"github.com/georgearnall/gh-monitor/internal/runs"
)

// isolate redirects statePath() to a per-test tempdir.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "gh-monitor", "state.json")
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
	original.LastNotifs = []notifs.Notification{
		{ID: "n1", Repo: "x/y", PRNumber: 1, Title: "Need review", Reason: "review_requested", URL: "https://github.com/x/y/pull/1", UpdatedAt: now, Unread: true},
		{ID: "n2", Repo: "x/y", PRNumber: 2, Title: "Reply", Reason: "comment", URL: "https://github.com/x/y/pull/2#issuecomment-9", UpdatedAt: now, Unread: false},
	}
	original.LastPoll = now
	original.LastRateLimit = ghclient.RateLimit{Limit: 5000, Remaining: 4823, ResetAt: now.Add(time.Hour)}
	original.EtagCache = map[string]ghclient.EtagEntry{
		"repos/x/y/actions/runs?per_page=10": {ETag: `"abc123"`, Body: []byte(`{"workflow_runs":[]}`)},
		"user/repos?sort=pushed":              {ETag: `"def456"`, Body: []byte(`[]`)},
	}
	original.SetFailedBuildAlerts(false)
	original.NotifyAllGitHub = true
	original.NotifyOwnPRComments = true
	original.ObserveNotifAlert("seed", now, true)                   // cold start: record, no fire
	original.ObserveNotifAlert("seed", now.Add(time.Minute), false) // warm: fires, irrelevant here

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
	if !reflect.DeepEqual(reloaded.LastNotifs, original.LastNotifs) {
		t.Errorf("LastNotifs differ:\n  got %+v\n  want %+v", reloaded.LastNotifs, original.LastNotifs)
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
	if reloaded.FailedBuildAlertsEnabled() {
		t.Errorf("NotifyFailedBuilds=false should survive save/load")
	}
	if !reloaded.NotifyAllGitHub || !reloaded.NotifyOwnPRComments {
		t.Errorf("NotifyAllGitHub/NotifyOwnPRComments should survive save/load")
	}
	// Compare via UpdatedAt.Equal rather than reflect.DeepEqual on the whole
	// map: AlertedAt is stamped with a local time.Now() inside
	// ObserveNotifAlert, and a JSON round-trip can change a time.Time's
	// internal representation (monotonic reading, *Location) without
	// changing the instant it represents, which would make DeepEqual an
	// unreliable comparison here (see how LastPoll is compared above).
	seed, ok := reloaded.AlertedNotifs["seed"]
	if !ok {
		t.Fatalf("AlertedNotifs[\"seed\"] missing after reload")
	}
	if !seed.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("AlertedNotifs[\"seed\"].UpdatedAt = %v, want %v", seed.UpdatedAt, now.Add(time.Minute))
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

func TestRecordDismiss_AndIsDismissed(t *testing.T) {
	isolate(t)
	s, _ := Load()
	id := "thread-1"
	originalAt := time.Now().Add(-1 * time.Hour)

	if s.IsDismissed(id, originalAt) {
		t.Errorf("expected IsDismissed=false for unrecorded id")
	}

	s.RecordDismiss(id, originalAt)

	// Same UpdatedAt as dismissed: suppressed.
	if !s.IsDismissed(id, originalAt) {
		t.Errorf("expected same-updatedAt to be dismissed")
	}
	// Older UpdatedAt (somehow): suppressed.
	if !s.IsDismissed(id, originalAt.Add(-1*time.Minute)) {
		t.Errorf("expected older-updatedAt to be dismissed")
	}
	// Newer UpdatedAt: NOT suppressed (genuine new activity).
	if s.IsDismissed(id, originalAt.Add(1*time.Minute)) {
		t.Errorf("newer updatedAt should bypass the filter")
	}
}

func TestPruneDismissedAbsent(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()

	// Three entries: two old enough to be eligible for pruning, one fresh.
	s.RecordDismiss("still-echoed", now)
	s.RecordDismiss("gone", now)
	s.RecordDismiss("fresh", now)

	// Force the first two past the minAge threshold; "fresh" stays recent.
	for _, id := range []string{"still-echoed", "gone"} {
		e := s.DismissedNotifs[id]
		e.DismissedAt = now.Add(-5 * time.Minute)
		s.DismissedNotifs[id] = e
	}

	present := map[string]bool{"still-echoed": true}
	s.PruneDismissedAbsent(present, 60*time.Second)

	if _, ok := s.DismissedNotifs["still-echoed"]; !ok {
		t.Errorf("entry still in poll response should be kept")
	}
	if _, ok := s.DismissedNotifs["gone"]; ok {
		t.Errorf("entry absent from response and past minAge should be pruned")
	}
	if _, ok := s.DismissedNotifs["fresh"]; !ok {
		t.Errorf("entry absent from response but inside minAge should be kept")
	}
}

func TestPruneDismissedAbsent_NilMapIsNoOp(t *testing.T) {
	isolate(t)
	s, _ := Load()
	// Should not panic.
	s.PruneDismissedAbsent(nil, 60*time.Second)
}

func TestSaveLoad_PreservesDismissedNotifs(t *testing.T) {
	isolate(t)
	original, _ := Load()
	now := time.Now().UTC().Truncate(time.Second)
	original.RecordDismiss("abc", now)
	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reloaded.IsDismissed("abc", now) {
		t.Errorf("expected dismissal record to survive save/load")
	}
}

func TestDismissRun_AndIsRunDismissed(t *testing.T) {
	isolate(t)
	s, _ := Load()

	if s.IsRunDismissed(1) {
		t.Errorf("expected IsRunDismissed=false for unknown id")
	}

	s.DismissRun(1)

	if !s.IsRunDismissed(1) {
		t.Errorf("expected IsRunDismissed=true after DismissRun")
	}
	if s.IsRunDismissed(2) {
		t.Errorf("expected IsRunDismissed=false for unrelated id")
	}
}

func TestPruneRunDismissals(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()

	s.DismissRun(1) // still in poll results
	s.DismissRun(2) // absent from poll results, old enough → drop
	s.DismissRun(3) // absent from poll results, too fresh → keep

	// Force ids 1 and 2 past the minAge threshold; id 3 stays recent.
	for _, id := range []int64{1, 2} {
		s.DismissedRuns[id] = now.Add(-5 * time.Minute)
	}

	present := map[int64]bool{1: true}
	s.PruneRunDismissals(present, 60*time.Second)

	if !s.IsRunDismissed(1) {
		t.Errorf("id 1 still in poll results: should be kept")
	}
	if s.IsRunDismissed(2) {
		t.Errorf("id 2 absent and past minAge: should be pruned")
	}
	if !s.IsRunDismissed(3) {
		t.Errorf("id 3 absent but inside minAge: should be kept")
	}
}

func TestSaveLoad_PreservesDismissedRuns(t *testing.T) {
	isolate(t)
	original, _ := Load()
	original.DismissRun(42)
	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reloaded.IsRunDismissed(42) {
		t.Errorf("expected dismissed run to survive save/load")
	}
	if reloaded.IsRunDismissed(99) {
		t.Errorf("undismissed run should not appear after reload")
	}
}

func TestLoad_InitializesDismissedRuns(t *testing.T) {
	isolate(t)
	s, _ := Load()
	if s.DismissedRuns == nil {
		t.Errorf("DismissedRuns should be non-nil after Load")
	}
}

func TestLoad_Missing_DefaultsFailedBuildAlertsOn(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.FailedBuildAlertsEnabled() {
		t.Errorf("fresh install should default failed-build alerts to on")
	}
}

func TestLoad_UpgradedStateWithoutField_DefaultsFailedBuildAlertsOn(t *testing.T) {
	path := isolate(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a state.json saved before NotifyFailedBuilds existed.
	if err := os.WriteFile(path, []byte(`{"runs":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.FailedBuildAlertsEnabled() {
		t.Errorf("state.json predating the field should default failed-build alerts to on")
	}
}

func TestLoad_PreservesExplicitFailedBuildAlertsFalse(t *testing.T) {
	isolate(t)
	original, _ := Load()
	original.SetFailedBuildAlerts(false)
	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.FailedBuildAlertsEnabled() {
		t.Errorf("explicit false should survive save/load, got enabled=true")
	}
}

func TestObserveNotifAlert_ColdStart_FirstSeen_NoFire(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()
	if got := s.ObserveNotifAlert("n1", now, true); got {
		t.Errorf("cold-start first observation got fire=true, want false")
	}
	if _, ok := s.AlertedNotifs["n1"]; !ok {
		t.Errorf("ObserveNotifAlert should record the id")
	}
}

// TestObserveNotifAlert_WarmLedger_NewIDFires is the fix for the case a
// naive mirror of Observe would miss: once the ledger is warm (not a cold
// start), a brand-new notification ID — e.g. the very first comment ever on
// a PR thread — must fire immediately, not wait for a second event on the
// same thread.
func TestObserveNotifAlert_WarmLedger_NewIDFires(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()
	if got := s.ObserveNotifAlert("n1", now, false); !got {
		t.Errorf("warm ledger, unknown id got fire=false, want true")
	}
}

func TestObserveNotifAlert_UnchangedUpdatedAt_NoRefire(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()
	s.ObserveNotifAlert("n1", now, true)
	if got := s.ObserveNotifAlert("n1", now, false); got {
		t.Errorf("unchanged updated_at got fire=true, want false (dedup)")
	}
}

func TestObserveNotifAlert_NewerUpdatedAt_Fires(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()
	s.ObserveNotifAlert("n1", now, true)
	if got := s.ObserveNotifAlert("n1", now.Add(1*time.Minute), false); !got {
		t.Errorf("newer updated_at got fire=false, want true")
	}
}

func TestPruneAlertedNotifsAbsent(t *testing.T) {
	isolate(t)
	s, _ := Load()
	now := time.Now()

	s.ObserveNotifAlert("still-present", now, true)
	s.ObserveNotifAlert("gone", now, true)
	s.ObserveNotifAlert("fresh", now, true)

	// Force the first two past the minAge threshold; "fresh" stays recent.
	for _, id := range []string{"still-present", "gone"} {
		e := s.AlertedNotifs[id]
		e.AlertedAt = now.Add(-5 * time.Minute)
		s.AlertedNotifs[id] = e
	}

	present := map[string]bool{"still-present": true}
	s.PruneAlertedNotifsAbsent(present, 60*time.Second)

	if _, ok := s.AlertedNotifs["still-present"]; !ok {
		t.Errorf("entry still in poll response should be kept")
	}
	if _, ok := s.AlertedNotifs["gone"]; ok {
		t.Errorf("entry absent from response and past minAge should be pruned")
	}
	if _, ok := s.AlertedNotifs["fresh"]; !ok {
		t.Errorf("entry absent from response but inside minAge should be kept")
	}
}

func TestPruneAlertedNotifsAbsent_NilMapIsNoOp(t *testing.T) {
	isolate(t)
	s, _ := Load()
	// Should not panic.
	s.PruneAlertedNotifsAbsent(nil, 60*time.Second)
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
