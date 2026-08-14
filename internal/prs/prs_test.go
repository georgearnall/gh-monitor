package prs

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/georgearnall/gh-monitor/internal/ghclient"
)

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

func TestPoll_FiltersDraftsAndBucketsChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "viewer") || !strings.Contains(string(body), "statusCheckRollup") {
			t.Errorf("query body missing expected fragments: %s", body)
		}
		fmt.Fprint(w, `{"data":{"viewer":{"pullRequests":{"nodes":[
			{
				"number": 100, "title":"Draft PR","url":"https://github.com/x/y/pull/100",
				"isDraft": true, "updatedAt":"2026-05-18T10:00:00Z", "headRefName":"draft",
				"reviewDecision": null, "reviews": {"totalCount":0}, "comments": {"totalCount":0},
				"repository": {"nameWithOwner":"x/y"},
				"commits": {"nodes":[]}
			},
			{
				"number": 1, "title":"All green","url":"https://github.com/x/y/pull/1",
				"isDraft": false, "updatedAt":"2026-05-18T12:00:00Z", "headRefName":"feat-green",
				"reviewDecision": "APPROVED", "reviews": {"totalCount":2}, "comments": {"totalCount":3},
				"repository": {"nameWithOwner":"x/y"},
				"commits": {"nodes":[{"commit":{"statusCheckRollup":{
					"state":"SUCCESS",
					"contexts":{"totalCount":2,"nodes":[
						{"__typename":"CheckRun","conclusion":"SUCCESS","status":"COMPLETED"},
						{"__typename":"StatusContext","state":"SUCCESS"}
					]}
				}}}]}
			},
			{
				"number": 2, "title":"Has failing","url":"https://github.com/x/y/pull/2",
				"isDraft": false, "updatedAt":"2026-05-18T11:00:00Z", "headRefName":"feat-broken",
				"reviewDecision": "CHANGES_REQUESTED", "reviews": {"totalCount":1}, "comments": {"totalCount":0},
				"repository": {"nameWithOwner":"x/y"},
				"commits": {"nodes":[{"commit":{"statusCheckRollup":{
					"state":"FAILURE",
					"contexts":{"totalCount":2,"nodes":[
						{"__typename":"CheckRun","conclusion":"FAILURE","status":"COMPLETED"},
						{"__typename":"CheckRun","conclusion":"SUCCESS","status":"COMPLETED"}
					]}
				}}}]}
			},
			{
				"number": 3, "title":"Pending","url":"https://github.com/x/y/pull/3",
				"isDraft": false, "updatedAt":"2026-05-18T13:00:00Z", "headRefName":"feat-running",
				"reviewDecision": "REVIEW_REQUIRED", "reviews": {"totalCount":0}, "comments": {"totalCount":0},
				"repository": {"nameWithOwner":"x/y"},
				"commits": {"nodes":[{"commit":{"statusCheckRollup":{
					"state":"PENDING",
					"contexts":{"totalCount":1,"nodes":[
						{"__typename":"CheckRun","status":"IN_PROGRESS"}
					]}
				}}}]}
			},
			{
				"number": 4, "title":"No checks","url":"https://github.com/x/y/pull/4",
				"isDraft": false, "updatedAt":"2026-05-18T09:00:00Z", "headRefName":"feat-nochecks",
				"reviewDecision": null, "reviews": {"totalCount":0}, "comments": {"totalCount":1},
				"repository": {"nameWithOwner":"x/y"},
				"commits": {"nodes":[{"commit":{"statusCheckRollup":null}}]}
			}
		]}}}}`)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	got, _, err := Poll(c, time.Time{})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Draft is filtered.
	if len(got) != 4 {
		t.Fatalf("got %d PRs, want 4 (draft should be filtered): %+v", len(got), got)
	}

	// Sort order: failing first, then pending, then by updated_at desc.
	if got[0].Number != 2 {
		t.Errorf("first should be failing #2, got #%d", got[0].Number)
	}
	if got[1].Number != 3 {
		t.Errorf("second should be pending #3, got #%d", got[1].Number)
	}

	byNum := map[int]PR{}
	for _, p := range got {
		byNum[p.Number] = p
	}

	// #1: all-pass with approval + comments
	if p := byNum[1]; p.State != "SUCCESS" || p.Passing != 2 || p.Failing != 0 || p.Pending != 0 ||
		p.ReviewDecision != "APPROVED" || p.ReviewCount != 2 || p.CommentCount != 3 {
		t.Errorf("PR#1: %+v", p)
	}
	// #2: failure with changes requested
	if p := byNum[2]; p.State != "FAILURE" || p.Failing != 1 || p.Passing != 1 ||
		p.ReviewDecision != "CHANGES_REQUESTED" {
		t.Errorf("PR#2: %+v", p)
	}
	// #3: pending IN_PROGRESS check counted in Pending
	if p := byNum[3]; p.State != "PENDING" || p.Pending != 1 {
		t.Errorf("PR#3: %+v", p)
	}
	// #4: no rollup object — all zeros, State empty
	if p := byNum[4]; p.State != "" || p.Total != 0 || p.CommentCount != 1 {
		t.Errorf("PR#4: %+v", p)
	}
}

func TestPoll_FiltersBySince(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	old := now.Add(-200 * 24 * time.Hour).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":{"viewer":{"pullRequests":{"nodes":[
			{"number":1,"title":"recent","url":"u1","isDraft":false,"updatedAt":%q,
			 "headRefName":"a","reviewDecision":"","reviews":{"totalCount":0},"comments":{"totalCount":0},
			 "repository":{"nameWithOwner":"x/y"},
			 "commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}},
			{"number":2,"title":"old","url":"u2","isDraft":false,"updatedAt":%q,
			 "headRefName":"b","reviewDecision":"","reviews":{"totalCount":0},"comments":{"totalCount":0},
			 "repository":{"nameWithOwner":"x/y"},
			 "commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}
		]}}}}`, recent, old)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	// Zero since: both kept.
	got, _, err := Poll(c, time.Time{})
	if err != nil {
		t.Fatalf("Poll(zero since): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("zero since should keep both; got %d", len(got))
	}

	// since = now - 60 days: drops the 200d-old PR.
	got, _, err = Poll(c, now.Add(-60*24*time.Hour))
	if err != nil {
		t.Fatalf("Poll(60d since): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("60d since should keep only the recent PR; got %d: %+v", len(got), got)
	}
	if got[0].Number != 1 {
		t.Errorf("expected to keep #1 (recent); got #%d", got[0].Number)
	}
}

func TestPoll_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"viewer not authorized"}]}`)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	if _, _, err := Poll(c, time.Time{}); err == nil {
		t.Error("expected error from graphql errors[], got nil")
	}
}
