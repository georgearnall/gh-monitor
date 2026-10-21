package main

import (
	"bufio"
	"context"
	"io"
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

// readKeys forwards stdin keystrokes to the keys channel. See readKeysFrom.
func readKeys(ctx context.Context, keys chan<- rune) {
	readKeysFrom(ctx, os.Stdin, keys)
}

// readKeysFrom forwards keystrokes from in to keys. Plain bytes are forwarded
// as-is. Arrow keys (CSI "ESC [ A".."D", or SS3 "ESC O A".."D" in application
// cursor mode) become the synthetic keyUp/Down/Right/Left runes. Other escape
// sequences, including ones with parameters such as Ctrl+arrow
// ("ESC [ 1 ; 5 A"), are consumed whole and dropped.
//
// A lone Esc is forwarded as 0x1B. It's told apart from the start of a
// sequence by whether more bytes arrived in the same read: terminals write a
// whole sequence at once, while a human pressing Esc produces a single byte.
func readKeysFrom(ctx context.Context, in io.Reader, keys chan<- rune) {
	r := bufio.NewReader(in)
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
		if r.Buffered() == 0 {
			if !forward(0x1B) {
				return
			}
			continue
		}
		next, err := r.ReadByte()
		if err != nil {
			return
		}
		var arrow rune
		switch next {
		case '[':
			// CSI: parameter/intermediate bytes then one final byte in
			// 0x40..0x7E. Only parameterless A-D are arrows.
			params := false
			for {
				b, err := r.ReadByte()
				if err != nil {
					return
				}
				if b >= 0x40 && b <= 0x7E {
					if !params {
						arrow = arrowFor(b)
					}
					break
				}
				params = true
			}
		case 'O':
			b, err := r.ReadByte()
			if err != nil {
				return
			}
			arrow = arrowFor(b)
		default:
			// Alt+key arrives as ESC <key>; treat it as the plain key.
			arrow = rune(next)
		}
		if arrow == 0 {
			continue
		}
		if !forward(arrow) {
			return
		}
	}
}

func arrowFor(b byte) rune {
	switch b {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		return keyRight
	case 'D':
		return keyLeft
	}
	return 0
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
