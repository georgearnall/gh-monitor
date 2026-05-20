package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/georgearnall/gha-monitor/internal/ghclient"
)

func TestDismissWorker_DrainsSequentially(t *testing.T) {
	// Simulates GitHub: returns 204 to every DELETE and records the
	// arrival times to prove we don't fire concurrently.
	var (
		mu       sync.Mutex
		arrivals []time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("want DELETE, got %s", r.Method)
		}
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		time.Sleep(10 * time.Millisecond) // simulate latency
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan dismissReq, 8)
	out := make(chan dismissOutcome, 8)
	go dismissWorker(ctx, c, in, out)

	in <- dismissReq{ID: "1"}
	in <- dismissReq{ID: "2"}
	in <- dismissReq{ID: "3"}

	for i := 0; i < 3; i++ {
		select {
		case o := <-out:
			if o.Err != nil {
				t.Errorf("dismiss %s errored: %v", o.ID, o.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for outcome %d", i+1)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 {
		t.Fatalf("got %d arrivals, want 3", len(arrivals))
	}
	// Each arrival should be at least ~10ms (one latency unit) after
	// the previous, since the worker is sequential.
	for i := 1; i < len(arrivals); i++ {
		gap := arrivals[i].Sub(arrivals[i-1])
		if gap < 5*time.Millisecond {
			t.Errorf("arrival %d came %v after arrival %d; expected serial execution", i, gap, i-1)
		}
	}
}

func TestDismissWithRetry_RetriesOn5xxThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Use the worker's retry helper directly to keep the test focused
	// on the retry semantics without involving the channel plumbing.
	err := dismissWithRetry(ctx, c, "abc")
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("got %d attempts, want 3 (two 503s then a 204)", got)
	}
}

func TestDismissWithRetry_DoesNotRetry4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := dismissWithRetry(ctx, c, "missing")
	if err == nil {
		t.Fatalf("expected error on 404, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("got %d attempts, want 1 (4xx should not retry)", got)
	}
}

func TestDismissWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := dismissWithRetry(ctx, c, "always-bad")
	if err == nil {
		t.Fatalf("expected eventual error, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != int32(dismissMaxAttempts) {
		t.Errorf("got %d attempts, want %d", got, dismissMaxAttempts)
	}
}

func TestDismissWithRetry_ContextCancelInBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately so the first backoff sleep aborts.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := dismissWithRetry(ctx, c, "x")
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}
}

func TestDismissWorker_ReportsError(t *testing.T) {
	// 404 is non-retryable, so the worker reports the error promptly.
	// Retry-on-5xx behaviour is covered by TestDismissWithRetry_* above.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := ghclient.NewForTest(srv.Client(), srv.URL+"/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan dismissReq, 1)
	out := make(chan dismissOutcome, 1)
	go dismissWorker(ctx, c, in, out)

	in <- dismissReq{ID: "fail-me"}
	select {
	case o := <-out:
		if o.Err == nil {
			t.Errorf("expected error for 404 response")
		}
		if o.ID != "fail-me" {
			t.Errorf("outcome.ID = %q, want fail-me", o.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outcome")
	}
}
