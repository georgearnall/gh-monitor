# Windows support

`gh-monitor` is developed and tested on macOS/Linux. Most of it works
fine cross-platform (it's plain Go, and the flag parsing / polling /
state layers have no OS-specific code), but the interactive TUI's raw
terminal input is currently **unfixed and unverified on Windows**.

## The problem: raw terminal input

Interactive single-key bindings (`↑`/`↓`, `q`, `m`, `M`, `d`, `r`)
depend on the terminal being in cbreak mode: no line buffering, no
local echo, so `readKeys` in `keys.go` can read stdin one byte at a
time and react immediately.

That mode is set by `sttyApply` in `internal/ui/ui.go`, which shells
out to the `stty` binary (`stty -echo -icanon`) from `EnterAltScreen`.
`stty` doesn't exist on a native Windows console (`cmd.exe` /
PowerShell without WSL or a POSIX layer). The command fails, the error
is discarded, and the terminal is left in canonical/echo mode:

- every keystroke gets echoed onto the alt-screen, corrupting the
  display
- single-key bindings don't register until Enter is pressed, because
  the terminal is still line-buffering input

Net effect: the keybinding-driven UI described in the README doesn't
work as designed on Windows.

## Why this isn't a one-line fix

`golang.org/x/term` (already a direct dependency, used elsewhere in
`ui.go` for `term.GetState`/`term.Restore`) has a genuinely
cross-platform `term.MakeRaw`, which looks like the obvious swap-in
replacement for the `stty` call. It probably is the right fix, but
landing it blind is risky for one reason: Windows consoles have
historically reported arrow/function keys through the Win32 console
input API rather than as ANSI CSI escape sequences (`ESC [ A` etc.),
unless the console has `ENABLE_VIRTUAL_TERMINAL_INPUT` set. Whether
that's already on by default depends on legacy conhost vs. modern
Windows Terminal / ConPTY, and isn't something `term.MakeRaw` alone
guarantees.

`readKeys` (`keys.go`) only understands CSI sequences — if arrow keys
arrive some other way, `term.MakeRaw` would fix echo/buffering but
arrow-key navigation would still silently fail. That's a real
console-input question that reading the code can't settle; it needs
someone to actually run the built binary in a Windows Terminal session
(and ideally legacy `cmd.exe`) and confirm arrow keys move the cursor
before this is called fixed.

## Status

Deliberately left unfixed in this pass. `TermWidth()` (terminal-size
detection) and `openURL()` (opening links in the browser) have been
switched to cross-platform implementations, and desktop
notifications/sound alerts are disabled outright on Windows (see the
README) rather than failing silently — but raw input mode is the one
piece that genuinely needs a Windows machine to verify, not just a
Windows cross-compile.

If you want to pick this up: swap `sttyApply`'s call site for
`term.MakeRaw(int(os.Stdin.Fd()))` in `EnterAltScreen`, then manually
test all bindings in both Windows Terminal and legacy `cmd.exe` before
merging.
