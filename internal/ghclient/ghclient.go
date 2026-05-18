package ghclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

const githubAPIBase = "https://api.github.com/"

// RateLimit holds the most recently observed REST rate-limit state.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
}

// Client wraps a go-gh authenticated http.Client, capturing rate-limit headers
// from every response, caching ETags so unchanged resources return 304 (which
// does not count against the primary REST budget), and surfacing a typed
// RateLimitedError when GitHub returns 403/429.
type Client struct {
	http *http.Client

	mu    sync.Mutex
	last  RateLimit
	etags map[string]etagEntry
}

type etagEntry struct {
	ETag string
	Body []byte
}

func New() (*Client, error) {
	hc, err := api.DefaultHTTPClient()
	if err != nil {
		return nil, err
	}
	return &Client{http: hc, etags: map[string]etagEntry{}}, nil
}

// Get performs an authenticated GET, transparently using a cached body when
// the server returns 304 Not Modified. Decodes the JSON response into v.
//
// Returns *RateLimitedError on 403/429.
func (c *Client) Get(path string, v any) error {
	req, err := http.NewRequest(http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	cached, hasCache := c.lookupEtag(path)
	if hasCache {
		req.Header.Set("If-None-Match", cached.ETag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	c.recordRateLimit(resp)

	switch {
	case resp.StatusCode == http.StatusNotModified && hasCache:
		if v == nil {
			return nil
		}
		return json.NewDecoder(bytes.NewReader(cached.Body)).Decode(v)

	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		retry := parseRetryAfter(resp.Header.Get("Retry-After"))
		body, _ := io.ReadAll(resp.Body)
		return &RateLimitedError{
			Status:     resp.StatusCode,
			RetryAfter: retry,
			Body:       string(body),
		}

	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, path, body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.storeEtag(path, etagEntry{ETag: etag, Body: body})
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(body, v)
}

// GraphQL posts an authenticated GraphQL query to /graphql and decodes the
// `data` portion of the response into v. Returns an error if the response
// contains GraphQL `errors[]` or is a rate-limit response.
func (c *Client) GraphQL(query string, vars map[string]any, v any) error {
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": vars,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, githubAPIBase+"graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	c.recordRateLimit(resp)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		raw, _ := io.ReadAll(resp.Body)
		return &RateLimitedError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       string(raw),
		}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, raw)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, v)
}

func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *Client) lookupEtag(path string) (etagEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.etags[path]
	return e, ok
}

func (c *Client) storeEtag(path string, e etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.etags[path] = e
}

func (c *Client) recordRateLimit(resp *http.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v := resp.Header.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.last.Limit = n
		}
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.last.Remaining = n
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.last.ResetAt = time.Unix(n, 0)
		}
	}
}

// RateLimitedError is returned when GitHub responds 403/429.
type RateLimitedError struct {
	Status     int
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited (HTTP %d); retry after %s", e.Status, e.RetryAfter)
}

// AsRateLimited checks if err is a *RateLimitedError and returns it.
func AsRateLimited(err error) (*RateLimitedError, bool) {
	var rl *RateLimitedError
	if errors.As(err, &rl) {
		return rl, true
	}
	return nil, false
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 30 * time.Second
	}
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 30 * time.Second
		}
		return d
	}
	return 30 * time.Second
}
