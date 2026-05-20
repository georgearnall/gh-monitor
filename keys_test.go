package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestCoalesceSignal_DebouncesBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan os.Signal, 100)
	out := coalesceSignal(ctx, in, 30*time.Millisecond)

	// A burst of signals (simulating a resize drag).
	for i := 0; i < 20; i++ {
		in <- os.Interrupt
	}

	// Exactly one event should arrive once the burst settles.
	select {
	case <-out:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected one debounced event after burst")
	}

	// No more events for at least the debounce window.
	select {
	case <-out:
		t.Errorf("received extra event after the burst settled")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestCoalesceSignal_SingleSignalStillFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan os.Signal, 1)
	out := coalesceSignal(ctx, in, 20*time.Millisecond)

	in <- os.Interrupt
	select {
	case <-out:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("expected debounced event for a lone signal")
	}
}

func TestCoalesceSignal_TwoSeparateBursts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan os.Signal, 10)
	out := coalesceSignal(ctx, in, 20*time.Millisecond)

	in <- os.Interrupt
	select {
	case <-out:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("first event missing")
	}

	// Wait beyond debounce, then a second burst should fire its own event.
	time.Sleep(60 * time.Millisecond)
	in <- os.Interrupt
	select {
	case <-out:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("second event missing")
	}
}

func TestCoalesceSignal_StopsOnCtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan os.Signal, 1)
	out := coalesceSignal(ctx, in, 20*time.Millisecond)
	cancel()

	// After cancellation, an inbound signal must not produce output (the
	// goroutine has returned and the timer was stopped).
	in <- os.Interrupt
	select {
	case <-out:
		t.Errorf("expected no output after ctx cancel")
	case <-time.After(80 * time.Millisecond):
	}
}
