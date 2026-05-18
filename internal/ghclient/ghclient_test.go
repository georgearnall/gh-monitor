package ghclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient wraps an httptest.Server's URL (which doesn't end in "/")
// with a trailing slash and constructs a ghclient pointed at it.
func newTestClient(srv *httptest.Server) *Client {
	return NewForTest(srv.Client(), srv.URL+"/")
}

func TestGet_DecodesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/x/y" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"y","stars":42}`)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	var got struct {
		Name  string `json:"name"`
		Stars int    `json:"stars"`
	}
	if err := c.Get("repos/x/y", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "y" || got.Stars != 42 {
		t.Errorf("decoded %+v, want {y 42}", got)
	}
}

func TestGet_RecordsRateLimit(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4823")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt.Unix()))
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.Get("user", nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	rl := c.RateLimit()
	if rl.Limit != 5000 || rl.Remaining != 4823 {
		t.Errorf("rate limit %+v, want Limit=5000 Remaining=4823", rl)
	}
	if !rl.ResetAt.Equal(resetAt) {
		t.Errorf("ResetAt = %v, want %v", rl.ResetAt, resetAt)
	}
}

func TestGet_EtagCachedOn304(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `{"value":42}`)
	}))
	defer srv.Close()

	c := newTestClient(srv)

	// First call: 200 + body cached
	var first struct {
		Value int `json:"value"`
	}
	if err := c.Get("thing", &first); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if first.Value != 42 {
		t.Errorf("first.Value = %d, want 42", first.Value)
	}

	// Second call: should send If-None-Match. Server replies 304, cached body
	// is replayed into the new v.
	var second struct {
		Value int `json:"value"`
	}
	if err := c.Get("thing", &second); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second.Value != 42 {
		t.Errorf("second.Value = %d, want 42 (cached replay failed)", second.Value)
	}
	if calls != 2 {
		t.Errorf("expected 2 round trips, got %d", calls)
	}
}

func TestGet_RateLimitedError_403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"slow down"}`)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	err := c.Get("anything", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	rl, ok := AsRateLimited(err)
	if !ok {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if rl.Status != http.StatusForbidden {
		t.Errorf("Status=%d, want 403", rl.Status)
	}
	if rl.RetryAfter != 60*time.Second {
		t.Errorf("RetryAfter=%v, want 60s", rl.RetryAfter)
	}
	if !strings.Contains(rl.Body, "slow down") {
		t.Errorf("Body=%q, expected to contain 'slow down'", rl.Body)
	}
}

func TestGet_RateLimitedError_429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := newTestClient(srv).Get("x", nil)
	if _, ok := AsRateLimited(err); !ok {
		t.Errorf("expected RateLimitedError, got %v", err)
	}
}

func TestGet_OtherHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	err := newTestClient(srv).Get("x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := AsRateLimited(err); ok {
		t.Errorf("500 should not be classed as rate-limited")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, expected status + body", err)
	}
}

func TestGraphQL_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["query"] != "{ viewer { login } }" {
			t.Errorf("query mismatch: %v", req["query"])
		}
		fmt.Fprint(w, `{"data":{"viewer":{"login":"georgearnall"}}}`)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	var got struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.GraphQL("{ viewer { login } }", nil, &got); err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if got.Viewer.Login != "georgearnall" {
		t.Errorf("got %+v", got)
	}
}

func TestGraphQL_ErrorsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"field unknown"},{"message":"deprecated"}]}`)
	}))
	defer srv.Close()

	err := newTestClient(srv).GraphQL("{}", nil, nil)
	if err == nil {
		t.Fatal("expected graphql error")
	}
	if !strings.Contains(err.Error(), "field unknown") || !strings.Contains(err.Error(), "deprecated") {
		t.Errorf("error = %q, expected both messages", err)
	}
}

func TestGraphQL_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := newTestClient(srv).GraphQL("{}", nil, nil)
	rl, ok := AsRateLimited(err)
	if !ok {
		t.Fatalf("expected RateLimitedError, got %v", err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", rl.RetryAfter)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 30 * time.Second},     // default
		{"0", 30 * time.Second},    // zero falls back to default
		{"5", 5 * time.Second},     // seconds
		{"120", 2 * time.Minute},   // seconds
		{"garbage", 30 * time.Second}, // unparseable falls back
		{"Wed, 21 Oct 2099 07:28:00 GMT", 0}, // HTTP-date in distant future returns time-until; we just sanity-check it's not the default
	}
	for _, c := range cases {
		got := parseRetryAfter(c.in)
		if c.in == "Wed, 21 Oct 2099 07:28:00 GMT" {
			if got == 30*time.Second {
				t.Errorf("HTTP-date should not fall back to default")
			}
			continue
		}
		if got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEtags_RoundTrip(t *testing.T) {
	c := NewForTest(http.DefaultClient, "http://unused/")
	if got := c.Etags(); len(got) != 0 {
		t.Errorf("expected empty initial cache, got %d entries", len(got))
	}

	c.SetEtags(map[string]EtagEntry{
		"repos/x/y": {ETag: `"abc"`, Body: []byte(`{"name":"y"}`)},
		"repos/a/b": {ETag: `"def"`, Body: []byte(`{"name":"b"}`)},
	})
	got := c.Etags()
	if len(got) != 2 {
		t.Fatalf("after SetEtags, len=%d want 2: %+v", len(got), got)
	}
	if got["repos/x/y"].ETag != `"abc"` {
		t.Errorf("Etag for repos/x/y = %q", got["repos/x/y"].ETag)
	}

	// Defensive copy: mutating the returned map shouldn't affect the client.
	delete(got, "repos/x/y")
	if c.Etags()["repos/x/y"].ETag != `"abc"` {
		t.Errorf("Etags() returned a shared map (mutation leaked back to client)")
	}
}

func TestSetEtags_NilOrEmptyIsNoOp(t *testing.T) {
	c := NewForTest(http.DefaultClient, "http://unused/")
	c.SetEtags(map[string]EtagEntry{"k": {ETag: `"v"`, Body: []byte("body")}})
	c.SetEtags(nil)
	c.SetEtags(map[string]EtagEntry{})
	if got := c.Etags(); len(got) != 1 {
		t.Errorf("nil/empty SetEtags should not clear; got %d entries", len(got))
	}
}

func TestSetEtags_SeedsTheCacheForCondGet(t *testing.T) {
	// Server REQUIRES If-None-Match: "v1" and returns 304. Anything else 500s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"v1"` {
			t.Errorf("server expected If-None-Match \"v1\", got %q", r.Header.Get("If-None-Match"))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := NewForTest(srv.Client(), srv.URL+"/")
	c.SetEtags(map[string]EtagEntry{
		"thing": {ETag: `"v1"`, Body: []byte(`{"value":99}`)},
	})

	var got struct {
		Value int `json:"value"`
	}
	if err := c.Get("thing", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != 99 {
		t.Errorf("got %+v, want value=99 (replayed from seeded cache)", got)
	}
}

func TestViewer_FetchesAndCaches(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/user" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"login":"georgearnall"}`)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	for i := 0; i < 3; i++ {
		login, err := c.Viewer()
		if err != nil {
			t.Fatalf("Viewer call %d: %v", i, err)
		}
		if login != "georgearnall" {
			t.Errorf("login = %q, want georgearnall", login)
		}
	}
	if calls != 1 {
		t.Errorf("expected /user to be hit once (cached), got %d", calls)
	}
}

func TestAsRateLimited_NotRateLimited(t *testing.T) {
	plain := errors.New("nope")
	if _, ok := AsRateLimited(plain); ok {
		t.Errorf("plain error misclassified as rate-limited")
	}
	if _, ok := AsRateLimited(nil); ok {
		t.Errorf("nil error misclassified")
	}
}
