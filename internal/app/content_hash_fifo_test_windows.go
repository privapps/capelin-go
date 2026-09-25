//go:build windows

package app

import (
	"errors"
	"os"
)

func makeContentHashFIFO(path string) error {
	return errors.New("content hash FIFO tests require Unix")
}

func openContentHashFIFOWriter(path string) (*os.File, error) {
	return nil, errors.New("content hash FIFO tests require Unix")
}

func contentHashFIFOUnavailable(error) bool {
	return false
}
