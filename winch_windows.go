//go:build windows

package main

import "os"

// notifyWinch is a no-op on Windows: there is no SIGWINCH and the TUI
// already doesn't function fully on Windows terminals. Returning a
// never-firing channel keeps the watch loop's select clean.
func notifyWinch() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal)
	return ch, func() {}
}
