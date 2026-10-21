package main

import (
	"context"
	"io"
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

func collectKeys(t *testing.T, chunks ...string) []rune {
	t.Helper()
	pr, pw := io.Pipe()
	keys := make(chan rune, 32)
	done := make(chan struct{})
	go func() {
		readKeysFrom(context.Background(), pr, keys)
		close(done)
	}()
	var got []rune
	for _, c := range chunks {
		pw.Write([]byte(c))
		// Drain what this write produced before the next one, so each chunk
		// is seen as its own read (as a human typing would produce).
	drain:
		for {
			select {
			case k := <-keys:
				got = append(got, k)
			case <-time.After(20 * time.Millisecond):
				break drain
			}
		}
	}
	pw.Close()
	<-done
	return got
}

func TestReadKeys_LoneEscIsForwarded(t *testing.T) {
	got := collectKeys(t, "\x1b", "q")
	if len(got) != 2 || got[0] != 0x1B || got[1] != 'q' {
		t.Errorf("got %q, want [ESC q]", got)
	}
}

func TestReadKeys_ArrowSequences(t *testing.T) {
	got := collectKeys(t, "\x1b[A", "\x1b[B", "\x1bOA", "\x1bOD")
	want := []rune{keyUp, keyDown, keyUp, keyLeft}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReadKeys_ParameterisedSequencesDropped(t *testing.T) {
	// Ctrl+Up, then Delete, then a plain key: only the plain key survives.
	got := collectKeys(t, "\x1b[1;5A", "\x1b[3~", "x")
	if len(got) != 1 || got[0] != 'x' {
		t.Errorf("got %q, want [x]", got)
	}
}
