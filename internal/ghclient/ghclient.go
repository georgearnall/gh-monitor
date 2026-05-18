package ghclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// RateLimit holds the most recently observed REST rate-limit state.
type RateLimit struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
}

// Client wraps a go-gh REST client, capturing rate-limit headers from every
// response and surfacing a typed RateLimitedError when GitHub returns 403/429.
type Client struct {
	rest *api.RESTClient

	mu   sync.Mutex
	last RateLimit
}

func New() (*Client, error) {
	r, err := api.DefaultRESTClient()
	if err != nil {
		return nil, err
	}
	return &Client{rest: r}, nil
}

// Get performs an authenticated GET and decodes the JSON body into v.
// Returns *RateLimitedError on 403/429 with Retry-After hint.
func (c *Client) Get(path string, v any) error {
	resp, err := c.rest.Request(http.MethodGet, path, nil)
	if err != nil {
		// go-gh returns a parsed error containing the response status for
		// non-2xx; surface it directly so callers can match on it.
		return err
	}
	defer resp.Body.Close()

	c.recordRateLimit(resp)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		retry := parseRetryAfter(resp.Header.Get("Retry-After"))
		body, _ := io.ReadAll(resp.Body)
		return &RateLimitedError{
			Status:     resp.StatusCode,
			RetryAfter: retry,
			Body:       string(body),
		}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, path, body)
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
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
