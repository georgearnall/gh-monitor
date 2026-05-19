//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyWinch wires SIGWINCH so the watch loop can redraw on terminal
// resize. The returned cleanup function stops delivery; callers should
// `defer` it on shutdown.
func notifyWinch() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}
