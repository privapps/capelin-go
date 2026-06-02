package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type sessionLogger struct {
	file       *os.File
	sessionID  string
	startTime  string
	exitReason string
	lastID     string
	closed     bool
	mu         sync.Mutex
}

func newSessionLogger(dir string) (*sessionLogger, error) {
	if dir == "" {
		dir = "."
	}

	start := time.Now().UTC()
	logDir := filepath.Join(dir, ".capelin-go", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session log dir: %w", err)
	}

	filename := strings.ReplaceAll(start.Truncate(time.Millisecond).Format(time.RFC3339Nano), ":", "-") + ".jsonl"
	file, err := os.OpenFile(filepath.Join(logDir, filename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening session log file: %w", err)
	}

	return &sessionLogger{
		file:       file,
		sessionID:  newUUID(),
		startTime:  start.Format(time.RFC3339),
		exitReason: "complete",
	}, nil
}

func (l *sessionLogger) emit(eventType string, data any) string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id, _ := l.emitLocked(eventType, data)
	return id
}

func (l *sessionLogger) emitLocked(eventType string, data any) (string, error) {
	if l == nil || l.file == nil || l.closed {
		return "", nil
	}

	id := newUUID()
	var parentID *string
	if l.lastID != "" {
		parent := l.lastID
		parentID = &parent
	}

	event := struct {
		Type      string  `json:"type"`
		ID        string  `json:"id"`
		Timestamp string  `json:"timestamp"`
		ParentID  *string `json:"parentId"`
		Data      any     `json:"data"`
	}{
		Type:      eventType,
		ID:        id,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		ParentID:  parentID,
		Data:      data,
	}

	raw, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	if _, err := l.file.Write(append(raw, '\n')); err != nil {
		return "", err
	}

	l.lastID = id
	return id, nil
}

func (l *sessionLogger) close() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}

	if l.exitReason == "" {
		l.exitReason = "complete"
	}

	var emitErr error
	if l.file != nil {
		_, emitErr = l.emitLocked("session.end", map[string]any{
			"exitReason": l.exitReason,
		})
	}

	var syncErr error
	var closeErr error
	if l.file != nil {
		syncErr = l.file.Sync()
		closeErr = l.file.Close()
		l.file = nil
	}
	l.closed = true

	if emitErr != nil {
		return emitErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%s",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		fmt.Sprintf("%x", b[10:16]),
	)
}
