package notifs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/georgearnall/gh-monitor/internal/ghclient"
)

func TestParsePRNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"https://api.github.com/repos/acme/billing/pulls/88", 88},
		{"https://api.github.com/repos/acme/billing/pulls/1", 1},
		{"", 0},
		{"https://api.github.com/repos/acme/billing/pulls/", 0},
		{"https://api.github.com/repos/acme/billing/pulls/abc", 0},
	}
	for _, c := range cases {
		if got := parsePRNumber(c.in); got != c.want {
			t.Errorf("parsePRNumber(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestApiCommentURLtoHTML(t *testing.T) {
	pr := "https://github.com/acme/billing/pull/88"
	cases := []struct {
		name string
		api  string
		want string
	}{
		{"issue comment", "https://api.github.com/repos/acme/billing/issues/comments/9999", pr + "#issuecomment-9999"},
		{"PR review comment", "https://api.github.com/repos/acme/billing/pulls/comments/4242", pr + "#discussion_r4242"},
		{"unparseable id", "https://api.github.com/repos/acme/billing/issues/comments/abc", ""},
		{"unknown shape", "https://api.github.com/repos/acme/billing/whatever/9", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := apiCommentURLtoHTML(c.api, pr); got != c.want {
				t.Errorf("apiCommentURLtoHTML = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildHTMLURL_PlainPRForNonComment(t *testing.T) {
	n := apiNotification{
		Reason: "review_requested",
	}
	n.Subject.LatestCommentURL = "https://api.github.com/repos/acme/billing/issues/comments/9"
	n.Repository.FullName = "acme/billing"
	if got := buildHTMLURL(n, 88); got != "https://github.com/acme/billing/pull/88" {
		t.Errorf("non-comment notification should not anchor: got %q", got)
	}
}

func TestBuildHTMLURL_AnchoredForComment(t *testing.T) {
	n := apiNotification{
		Reason: "comment",
	}
	n.Subject.LatestCommentURL = "https://api.github.com/repos/acme/billing/issues/comments/9999"
	n.Repository.FullName = "acme/billing"
	want := "https://github.com/acme/billing/pull/88#issuecomment-9999"
	if got := buildHTMLURL(n, 88); got != want {
		t.Errorf("comment notification: got %q want %q", got, want)
	}
}

func TestBuildHTMLURL_FallbackOnUnparseableComment(t *testing.T) {
	n := apiNotification{
		Reason: "comment",
	}
	n.Subject.LatestCommentURL = "https://api.github.com/something/weird"
	n.Repository.FullName = "acme/billing"
	if got := buildHTMLURL(n, 88); got != "https://github.com/acme/billing/pull/88" {
		t.Errorf("unparseable comment url should fall back to plain PR url: got %q", got)
	}
}

// TestPoll_FiltersAndSorts validates the full Poll pipeline:
//   - drops issues
//   - drops disallowed reasons (subscribed)
//   - drops read items older than 7 days
//   - keeps read items inside the 7-day window (dimmed downstream)
//   - sorts unread-first then by UpdatedAt desc
//   - anchors the URL for comment notifications
func TestPoll_FiltersAndSorts(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)
	older := now.Add(-2 * time.Hour).Format(time.RFC3339)
	ancient := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notifications" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.RawQuery != "all=true&per_page=50" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		fmt.Fprintf(w, `[
			{"id":"1","unread":true,"reason":"review_requested","updated_at":%q,
			 "subject":{"type":"PullRequest","title":"Add VAT","url":"https://api.github.com/repos/acme/billing/pulls/88"},
			 "repository":{"full_name":"acme/billing"}},
			{"id":"2","unread":true,"reason":"comment","updated_at":%q,
			 "subject":{"type":"PullRequest","title":"Tidy types","url":"https://api.github.com/repos/acme/checkout/pulls/234","latest_comment_url":"https://api.github.com/repos/acme/checkout/issues/comments/9999"},
			 "repository":{"full_name":"acme/checkout"}},
			{"id":"3","unread":false,"reason":"mention","updated_at":%q,
			 "subject":{"type":"PullRequest","title":"Old read mention","url":"https://api.github.com/repos/acme/legacy/pulls/35"},
			 "repository":{"full_name":"acme/legacy"}},
			{"id":"4","unread":false,"reason":"mention","updated_at":%q,
			 "subject":{"type":"PullRequest","title":"Ancient read","url":"https://api.github.com/repos/acme/legacy/pulls/12"},
			 "repository":{"full_name":"acme/legacy"}},
			{"id":"5","unread":true,"reason":"subscribed","updated_at":%q,
			 "subject":{"type":"PullRequest","title":"Drop this","url":"https://api.github.com/repos/acme/noise/pulls/9"},
			 "repository":{"full_name":"acme/noise"}},
			{"id":"6","unread":true,"reason":"mention","updated_at":%q,
			 "subject":{"type":"Issue","title":"Issue mention","url":"https://api.github.com/repos/acme/billing/issues/77"},
			 "repository":{"full_name":"acme/billing"}}
		]`, recent, older, recent, ancient, recent, recent)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	got, err := Poll(c)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Want kept: 1 (unread review_requested), 2 (unread comment), 3 (read mention, recent).
	// Want dropped: 4 (read mention, 30d old), 5 (subscribed reason), 6 (Issue).
	if len(got) != 3 {
		t.Fatalf("got %d notifications, want 3: %+v", len(got), got)
	}

	// Unread first; within unread, more recent first (id=1 at recent > id=2 at older).
	if got[0].ID != "1" || got[1].ID != "2" || got[2].ID != "3" {
		t.Errorf("sort order wrong: got %q,%q,%q want 1,2,3", got[0].ID, got[1].ID, got[2].ID)
	}

	// Comment notification should anchor to issuecomment.
	wantURL := "https://github.com/acme/checkout/pull/234#issuecomment-9999"
	if got[1].URL != wantURL {
		t.Errorf("comment URL = %q, want %q", got[1].URL, wantURL)
	}

	// Plain PR URL for the review_requested item.
	if got[0].URL != "https://github.com/acme/billing/pull/88" {
		t.Errorf("plain PR URL = %q", got[0].URL)
	}
	if got[0].PRNumber != 88 {
		t.Errorf("PR number = %d, want 88", got[0].PRNumber)
	}
	if got[0].Repo != "acme/billing" {
		t.Errorf("repo = %q, want acme/billing", got[0].Repo)
	}
	if !got[0].Unread {
		t.Errorf("expected got[0] unread")
	}
	if got[2].Unread {
		t.Errorf("expected got[2] (id=3) to be read")
	}
}

func TestPoll_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	got, err := Poll(c)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero notifications on empty response, got %+v", got)
	}
}

func TestMarkAllRead_FansOutToEachID(t *testing.T) {
	var (
		mu  sync.Mutex
		hit []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("want PATCH, got %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/notifications/threads/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		mu.Lock()
		hit = append(hit, strings.TrimPrefix(r.URL.Path, "/notifications/threads/"))
		mu.Unlock()
		w.WriteHeader(http.StatusResetContent)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	if err := MarkAllRead(c, []string{"1", "2", "3"}); err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hit) != 3 {
		t.Errorf("got %d PATCHes, want 3: %v", len(hit), hit)
	}
	seen := map[string]bool{}
	for _, id := range hit {
		seen[id] = true
	}
	for _, want := range []string{"1", "2", "3"} {
		if !seen[want] {
			t.Errorf("missing PATCH for id %q", want)
		}
	}
}

func TestMarkAllRead_EmptyIDs_NoRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("expected no requests, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	if err := MarkAllRead(c, nil); err != nil {
		t.Errorf("MarkAllRead(nil): %v", err)
	}
}

func TestMarkAllRead_ReturnsFirstError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	err := MarkAllRead(c, []string{"1", "2"})
	if err == nil {
		t.Errorf("expected error on 500, got nil")
	}
}

func TestDismissAll_FansOutToEachID(t *testing.T) {
	var (
		mu  sync.Mutex
		hit []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("want DELETE, got %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/notifications/threads/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		mu.Lock()
		hit = append(hit, strings.TrimPrefix(r.URL.Path, "/notifications/threads/"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	if err := DismissAll(c, []string{"1", "2", "3"}); err != nil {
		t.Fatalf("DismissAll: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hit) != 3 {
		t.Errorf("got %d DELETEs, want 3: %v", len(hit), hit)
	}
}

func TestDismissAll_EmptyIDs_NoRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("expected no requests, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	if err := DismissAll(c, nil); err != nil {
		t.Errorf("DismissAll(nil): %v", err)
	}
}

func TestDismissAll_ReturnsFirstError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	err := DismissAll(c, []string{"1", "2"})
	if err == nil {
		t.Errorf("expected error on 500, got nil")
	}
}

func TestFetchPRStates_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		// Mirror what GitHub returns for our aliased query. We don't
		// parse the query body here; just respond with the shape the
		// caller expects, keyed on the aliases pr0/pr1/pr2.
		fmt.Fprint(w, `{"data":{
			"pr0": {"pullRequest":{"state":"OPEN","isDraft":false}},
			"pr1": {"pullRequest":{"state":"MERGED","isDraft":false}},
			"pr2": {"pullRequest":{"state":"OPEN","isDraft":true}}
		}}`)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	ns := []Notification{
		{ID: "n0", Repo: "a/b", PRNumber: 1},
		{ID: "n1", Repo: "c/d", PRNumber: 2},
		{ID: "n2", Repo: "e/f", PRNumber: 3},
	}
	states, err := FetchPRStates(c, ns)
	if err != nil {
		t.Fatalf("FetchPRStates: %v", err)
	}
	if states["n0"] != PRStateOpen {
		t.Errorf("n0 = %q, want OPEN", states["n0"])
	}
	if states["n1"] != PRStateMerged {
		t.Errorf("n1 = %q, want MERGED", states["n1"])
	}
	// isDraft=true overrides state -> DRAFT.
	if states["n2"] != PRStateDraft {
		t.Errorf("n2 = %q, want DRAFT (isDraft override)", states["n2"])
	}
}

func TestFetchPRStates_EmptyNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("expected no request, got %s", r.URL.Path)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	if _, err := FetchPRStates(c, nil); err != nil {
		t.Errorf("FetchPRStates(nil): %v", err)
	}
}

func TestFetchPRStates_PartialResolveLeavesMissingOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only pr0 resolved; pr1 came back null (e.g. private repo not
		// visible to this token). Use the best-effort path that ignores
		// per-field errors.
		fmt.Fprint(w, `{"data":{
			"pr0": {"pullRequest":{"state":"OPEN","isDraft":false}},
			"pr1": null
		},"errors":[{"message":"Could not resolve"}]}`)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	ns := []Notification{
		{ID: "n0", Repo: "a/b", PRNumber: 1},
		{ID: "n1", Repo: "c/d", PRNumber: 2},
	}
	states, err := FetchPRStates(c, ns)
	if err != nil {
		t.Fatalf("FetchPRStates: %v", err)
	}
	if states["n0"] != PRStateOpen {
		t.Errorf("n0 = %q, want OPEN", states["n0"])
	}
	if _, ok := states["n1"]; ok {
		t.Errorf("n1 should be missing from the map; got %q", states["n1"])
	}
}

func TestFetchPRStates_SkipsMalformedNotifs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("expected no request when all notifs are malformed")
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	ns := []Notification{
		{ID: "n0"},                 // empty repo
		{ID: "n1", Repo: "noslash"}, // no slash in repo
		{ID: "n2", Repo: "a/b"},    // zero PRNumber
	}
	if _, err := FetchPRStates(c, ns); err != nil {
		t.Errorf("FetchPRStates: %v", err)
	}
}

func TestPoll_HTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")
	if _, err := Poll(c); err == nil {
		t.Errorf("expected error on 500, got nil")
	}
}
