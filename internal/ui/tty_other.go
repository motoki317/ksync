//go:build !unix

package ui

import (
	"errors"
	"os"
)

// drainTTY is a no-op where the unix non-blocking primitives are unavailable; the
// deadline-based branch of flushInput covers the supported terminals there.
func drainTTY(*os.File) {}

// pollReader is unavailable off unix, so ConfirmBuilds falls back to a blocking
// read loop (ctx/abort honored only when a key arrives).
func pollReader(int) (func([]byte) (int, error), func(), error) {
	return nil, nil, errors.New("non-blocking reads unsupported on this platform")
}
