//go:build !windows

package tools

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockEditFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockEditFile(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
