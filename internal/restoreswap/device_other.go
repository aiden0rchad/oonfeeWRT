//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package restoreswap

import "os"

// Root.Rename still rejects a cross-volume prepared directory on platforms
// whose FileInfo does not expose a portable device identifier.
func sameDevice(_, _ os.FileInfo) bool { return true }
