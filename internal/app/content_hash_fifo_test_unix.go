//go:build !windows

package app

import (
	"errors"
	"os"
	"syscall"
)

func makeContentHashFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

func openContentHashFIFOWriter(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func contentHashFIFOUnavailable(err error) bool {
	return errors.Is(err, syscall.ENXIO)
}
