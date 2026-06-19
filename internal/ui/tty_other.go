//go:build !unix

package ui

import "os"

// drainTTY is a no-op where the unix non-blocking primitives are unavailable; the
// deadline-based branch of flushInput covers the supported terminals there.
func drainTTY(*os.File) {}
