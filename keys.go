package main

import (
	"bufio"
	"context"
	"os"
	"time"
)

// Synthetic rune values for non-character keys. Chosen from the Unicode
// private-use area so they can flow through a chan rune alongside ASCII.
const (
	keyUp    rune = 0xE001
	keyDown  rune = 0xE002
	keyRight rune = 0xE003
	keyLeft  rune = 0xE004
)

// readKeys forwards stdin keystrokes to the keys channel. Plain ASCII bytes
// are forwarded as-is. CSI escape sequences (ESC [ A/B/C/D) are parsed into
// the synthetic keyUp/Down/Right/Left runes so the consumer can switch on
// them like any other key. Unknown CSI sequences are dropped silently.
func readKeys(ctx context.Context, keys chan<- rune) {
	r := bufio.NewReader(os.Stdin)
	forward := func(k rune) bool {
		select {
		case keys <- k:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		c, err := r.ReadByte()
		if err != nil {
			return
		}
		if c != 0x1B {
			if !forward(rune(c)) {
				return
			}
			continue
		}
		// ESC: try to consume a CSI sequence.
		next, err := r.ReadByte()
		if err != nil {
			return
		}
		if next != '[' {
			// Lone ESC or an unrelated escape; forward the second byte as a
			// regular keypress so the user's follow-up still registers.
			if !forward(rune(next)) {
				return
			}
			continue
		}
		third, err := r.ReadByte()
		if err != nil {
			return
		}
		var arrow rune
		switch third {
		case 'A':
			arrow = keyUp
		case 'B':
			arrow = keyDown
		case 'C':
			arrow = keyRight
		case 'D':
			arrow = keyLeft
		default:
			continue
		}
		if !forward(arrow) {
			return
		}
	}
}

// coalesceSignal forwards events from in to the returned channel, collapsing
// a burst of signals into a single event delivered once the burst has been
// quiet for d. Used to debounce SIGWINCH during a resize drag, which can
// fire 30+ times a second and produce visible flicker if each one triggers
// a full screen redraw.
//
// Cancels cleanly when ctx is done.
func coalesceSignal(ctx context.Context, in <-chan os.Signal, d time.Duration) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		var timer *time.Timer
		fire := func() {
			select {
			case out <- struct{}{}:
			default:
			}
		}
		for {
			select {
			case <-in:
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(d, fire)
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			}
		}
	}()
	return out
}
