package runs

import "testing"

func TestRun_IsActive(t *testing.T) {
	active := []string{"queued", "in_progress", "waiting", "pending", "requested"}
	terminal := []string{"completed", "", "failure", "success", "cancelled"}

	for _, s := range active {
		if !(Run{Status: s}).IsActive() {
			t.Errorf("IsActive(%q) = false, want true", s)
		}
	}
	for _, s := range terminal {
		if (Run{Status: s}).IsActive() {
			t.Errorf("IsActive(%q) = true, want false", s)
		}
	}
}

func TestRun_IsFailure(t *testing.T) {
	failures := []string{"failure", "timed_out", "startup_failure"}
	nonFailures := []string{"success", "cancelled", "neutral", "skipped", ""}

	for _, c := range failures {
		if !(Run{Conclusion: c}).IsFailure() {
			t.Errorf("IsFailure(%q) = false, want true", c)
		}
	}
	for _, c := range nonFailures {
		if (Run{Conclusion: c}).IsFailure() {
			t.Errorf("IsFailure(%q) = true, want false", c)
		}
	}
}
