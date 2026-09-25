package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// editFileLocks protects callers in this process. The OS lock acquired below
// extends the same per-file critical section to other Capelin processes.
var editFileLocks sync.Map

type editFileLock struct {
	local *sync.Mutex
	file  *os.File
}

func acquireEditFileLock(path string) (*editFileLock, error) {
	key, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve edit lock path: %w", err)
	}
	value, _ := editFileLocks.LoadOrStore(filepath.Clean(key), &sync.Mutex{})
	local := value.(*sync.Mutex)
	local.Lock()

	lockPath, err := editLockPath(key)
	if err != nil {
		local.Unlock()
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		local.Unlock()
		return nil, fmt.Errorf("open edit lock: %w", err)
	}
	if err := lockEditFile(file); err != nil {
		_ = file.Close()
		local.Unlock()
		return nil, fmt.Errorf("lock edit file: %w", err)
	}
	return &editFileLock{local: local, file: file}, nil
}

func (lock *editFileLock) release() {
	if lock == nil {
		return
	}
	unlockEditFile(lock.file)
	_ = lock.file.Close()
	lock.local.Unlock()
}

func editLockPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve edit lock path: %w", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(absolute)))
	directory := filepath.Join(os.TempDir(), "capelin-go-edit-locks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create edit lock directory: %w", err)
	}
	return filepath.Join(directory, hex.EncodeToString(sum[:])+".lock"), nil
}
