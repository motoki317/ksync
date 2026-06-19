//go:build unix

package ui

import (
	"os"
	"syscall"
)

// drainTTY discards queued input on a terminal whose os.File does not support read
// deadlines (every macOS tty): it switches the fd to non-blocking, reads until the
// queue drains (EAGAIN), then restores blocking mode. It is only called after a
// SetReadDeadline failure, which means the runtime poller is not managing this fd,
// so toggling O_NONBLOCK directly cannot disturb os.File's own non-blocking state —
// and the subsequent blocking reads in ConfirmBuilds see the fd restored.
func drainTTY(in *os.File) {
	fd := int(in.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		return
	}
	defer func() { _ = syscall.SetNonblock(fd, false) }()
	buf := make([]byte, 256)
	for {
		n, err := syscall.Read(fd, buf)
		if n <= 0 || err != nil { // EAGAIN once nothing is queued
			return
		}
	}
}
