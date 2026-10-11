//go:build !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !linux

package record

// isTerminal has no termios ioctl to ask on this platform, so it answers no: sessionKind then
// reads a run as headless unless CLAUDE_CODE_SESSION_ATTENDED says otherwise, which it does in
// every Claude Code session.
func isTerminal(uintptr) bool { return false }
