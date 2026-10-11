package record

import (
	"syscall"
	"unsafe"
)

// isTerminal is Python's isatty(): a terminal answers the termios ioctl, and nothing else does.
// Mode bits cannot stand in for it — /dev/null is a character device and not a tty.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
