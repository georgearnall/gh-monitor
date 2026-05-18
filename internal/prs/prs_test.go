package prs

import "testing"

func TestPR_IsPredicates(t *testing.T) {
	cases := []struct {
		state                       string
		failing, pending, passing bool
	}{
		{"SUCCESS", false, false, true},
		{"FAILURE", true, false, false},
		{"ERROR", true, false, false},
		{"PENDING", false, true, false},
		{"EXPECTED", false, true, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		p := PR{State: c.state}
		if got := p.IsFailing(); got != c.failing {
			t.Errorf("State=%q IsFailing=%v want %v", c.state, got, c.failing)
		}
		if got := p.IsPending(); got != c.pending {
			t.Errorf("State=%q IsPending=%v want %v", c.state, got, c.pending)
		}
		if got := p.IsPassing(); got != c.passing {
			t.Errorf("State=%q IsPassing=%v want %v", c.state, got, c.passing)
		}
	}
}

func TestBucket_CheckRun(t *testing.T) {
	cases := []struct {
		name                                   string
		status, conclusion                     string
		wantPassing, wantFailing, wantPending int
	}{
		{"in-progress is pending", "IN_PROGRESS", "", 0, 0, 1},
		{"queued is pending", "QUEUED", "", 0, 0, 1},
		{"completed success", "COMPLETED", "SUCCESS", 1, 0, 0},
		{"completed failure", "COMPLETED", "FAILURE", 0, 1, 0},
		{"completed timed_out", "COMPLETED", "TIMED_OUT", 0, 1, 0},
		{"completed startup_failure", "COMPLETED", "STARTUP_FAILURE", 0, 1, 0},
		{"completed cancelled is failing", "COMPLETED", "CANCELLED", 0, 1, 0},
		{"completed neutral counts as passing", "COMPLETED", "NEUTRAL", 1, 0, 0},
		{"completed skipped counts as passing", "COMPLETED", "SKIPPED", 1, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var pr PR
			bucket(contextNode{Typename: "CheckRun", Status: c.status, Conclusion: c.conclusion}, &pr)
			if pr.Passing != c.wantPassing || pr.Failing != c.wantFailing || pr.Pending != c.wantPending {
				t.Errorf("got pass=%d fail=%d pending=%d, want pass=%d fail=%d pending=%d",
					pr.Passing, pr.Failing, pr.Pending,
					c.wantPassing, c.wantFailing, c.wantPending)
			}
		})
	}
}

func TestBucket_StatusContext(t *testing.T) {
	cases := []struct {
		state                                  string
		wantPassing, wantFailing, wantPending int
	}{
		{"SUCCESS", 1, 0, 0},
		{"FAILURE", 0, 1, 0},
		{"ERROR", 0, 1, 0},
		{"PENDING", 0, 0, 1},
		{"EXPECTED", 0, 0, 1},
	}
	for _, c := range cases {
		var pr PR
		bucket(contextNode{Typename: "StatusContext", State: c.state}, &pr)
		if pr.Passing != c.wantPassing || pr.Failing != c.wantFailing || pr.Pending != c.wantPending {
			t.Errorf("State=%q got pass=%d fail=%d pending=%d, want pass=%d fail=%d pending=%d",
				c.state, pr.Passing, pr.Failing, pr.Pending,
				c.wantPassing, c.wantFailing, c.wantPending)
		}
	}
}

func TestBucket_UnknownTypename(t *testing.T) {
	var pr PR
	bucket(contextNode{Typename: "SomeNewType", State: "SUCCESS"}, &pr)
	if pr.Passing+pr.Failing+pr.Pending != 0 {
		t.Errorf("unknown typename should be no-op, got %+v", pr)
	}
}
