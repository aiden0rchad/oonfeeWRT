//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package restoreswap

import (
	"os"
	"syscall"
)

func sameDevice(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && uint64(leftStat.Dev) == uint64(rightStat.Dev)
}
