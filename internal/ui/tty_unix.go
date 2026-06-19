//go:build unix

package ui

import (
	"os"
	"syscall"
)

// pollReader switches a terminal fd to non-blocking mode and returns a read that
// yields whatever is queued right now (n==0, nil when nothing is) — so the picker
// can poll for keystrokes between ctx/abort checks without SetReadDeadline, which
// no tty supports. restore returns the fd to blocking mode and MUST run on every
// exit path: a leaked non-blocking stdin makes the parent shell error after ksync
// exits. Safe because pollReader is reached only once MakeRaw has succeeded — i.e.
// a real terminal, an fd the runtime poller does not manage — so toggling
// O_NONBLOCK behind os.File cannot disturb its state (same basis as drainTTY).
//
// A non-blocking read with nothing queued reports EAGAIN; n==0 with no error means
// EOF (the terminal closed). Both are folded to "no data, poll again" — exactly as
// drainTTY treats n<=0 — rather than relying on EAGAIN to be the only empty signal:
// the freeze bug was a tty where os.File semantics differed from a pipe's, so the
// picker must not vanish if some terminal returns 0 for empty. A genuinely closed
// stdin then ends the prompt via ctx (SIGHUP/SIGTERM), not a misread empty poll.
func pollReader(fd int) (read func([]byte) (int, error), restore func(), err error) {
	if err := syscall.SetNonblock(fd, true); err != nil {
		return nil, nil, err
	}
	read = func(buf []byte) (int, error) {
		n, err := syscall.Read(fd, buf)
		switch {
		case err == syscall.EAGAIN || err == syscall.EWOULDBLOCK:
			return 0, nil // nothing queued right now
		case err != nil:
			return 0, err
		default:
			return n, nil // n>=0; n==0 (EOF) folds to "no data" — see above
		}
	}
	restore = func() { _ = syscall.SetNonblock(fd, false) }
	return read, restore, nil
}

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
