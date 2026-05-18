package runs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/georgearnall/gha-monitor/internal/discovery"
	"github.com/georgearnall/gha-monitor/internal/ghclient"
)

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

func TestPoll_FansOutToEachRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/x/a/actions/runs"):
			fmt.Fprint(w, `{"workflow_runs":[
				{"id":1,"name":"CI","head_branch":"main","status":"in_progress","conclusion":"","html_url":"https://github.com/x/a/runs/1","actor":{"login":"alice"},"created_at":"2026-05-18T10:00:00Z","updated_at":"2026-05-18T10:01:00Z"}
			]}`)
		case strings.HasPrefix(r.URL.Path, "/repos/x/b/actions/runs"):
			fmt.Fprint(w, `{"workflow_runs":[
				{"id":2,"name":"Deploy","head_branch":"main","status":"completed","conclusion":"failure","html_url":"https://github.com/x/b/runs/2","actor":{"login":"bob"},"created_at":"2026-05-18T09:00:00Z","updated_at":"2026-05-18T09:30:00Z"},
				{"id":3,"name":"Tests","head_branch":"feature","status":"completed","conclusion":"success","html_url":"https://github.com/x/b/runs/3","actor":{"login":"alice"},"created_at":"2026-05-18T08:00:00Z","updated_at":"2026-05-18T08:15:00Z"}
			]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	repos := []discovery.Repo{
		{FullName: "x/a", Owner: "x", Name: "a"},
		{FullName: "x/b", Owner: "x", Name: "b"},
	}
	got, err := Poll(c, repos)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d runs, want 3: %+v", len(got), got)
	}

	byID := map[int64]Run{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if r := byID[1]; r.Repo != "x/a" || !r.IsActive() || r.ActorLogin != "alice" {
		t.Errorf("run 1: %+v", r)
	}
	if r := byID[2]; r.Repo != "x/b" || !r.IsFailure() || r.ActorLogin != "bob" {
		t.Errorf("run 2: %+v", r)
	}
	if r := byID[3]; r.Repo != "x/b" || r.Conclusion != "success" || r.ActorLogin != "alice" {
		t.Errorf("run 3: %+v", r)
	}
}

func TestPoll_EmptyRepos(t *testing.T) {
	c := ghclient.NewForTest(http.DefaultClient, "http://unused/")
	got, err := Poll(c, nil)
	if err != nil {
		t.Errorf("expected nil err on empty repos, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero runs, got %+v", got)
	}
}
