package main

import (
	"capelin-go/internal/types"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	sessionStateDir       = ".capelin-go"
	sessionSnapshotsDir   = "sessions"
	sessionSnapshotSuffix = ".json"
)

// sessionSnapshot is the durable representation of an interactive
// conversation. The field names intentionally match the existing snapshots
// produced by earlier capelin-go versions.
type sessionSnapshot struct {
	SessionUUID  string          `json:"sessionUUID"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	LastContent  string          `json:"lastContent,omitempty"`
	Name         string          `json:"name,omitempty"`
	Topic        string          `json:"topic,omitempty"`
	LastInput    string          `json:"lastInput,omitempty"`
	MessageCount int             `json:"messageCount"`
	Messages     []types.Message `json:"messages"`
	Todos        []todoItem      `json:"todos"`
}

// UnmarshalJSON accepts the original camelCase snapshot fields and a few
// conventional aliases used by early development snapshots. In particular,
// adding todo persistence must not make an older conversation unloadable.
func (s *sessionSnapshot) UnmarshalJSON(data []byte) error {
	var wire struct {
		SessionUUID    string          `json:"sessionUUID"`
		SessionID      string          `json:"session_id"`
		ID             string          `json:"id"`
		CreatedAt      time.Time       `json:"createdAt"`
		CreatedAtSnake time.Time       `json:"created_at"`
		UpdatedAt      time.Time       `json:"updatedAt"`
		UpdatedAtSnake time.Time       `json:"updated_at"`
		LastContent    string          `json:"lastContent"`
		LastResponse   string          `json:"last_response"`
		Name           string          `json:"name"`
		Topic          string          `json:"topic"`
		LastInput      string          `json:"lastInput"`
		LastInputSnake string          `json:"last_input"`
		MessageCount   int             `json:"messageCount"`
		Messages       []types.Message `json:"messages"`
		Todos          []todoItem      `json:"todos"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	s.SessionUUID = wire.SessionUUID
	if s.SessionUUID == "" {
		s.SessionUUID = wire.SessionID
	}
	if s.SessionUUID == "" {
		s.SessionUUID = wire.ID
	}
	s.CreatedAt = wire.CreatedAt
	if s.CreatedAt.IsZero() {
		s.CreatedAt = wire.CreatedAtSnake
	}
	s.UpdatedAt = wire.UpdatedAt
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = wire.UpdatedAtSnake
	}
	s.LastContent = wire.LastContent
	if s.LastContent == "" {
		s.LastContent = wire.LastResponse
	}
	s.Name = strings.TrimSpace(wire.Name)
	s.Topic = strings.TrimSpace(wire.Topic)
	s.LastInput = strings.TrimSpace(wire.LastInput)
	if s.LastInput == "" {
		s.LastInput = strings.TrimSpace(wire.LastInputSnake)
	}
	s.MessageCount = wire.MessageCount
	s.Messages = wire.Messages
	s.Todos = wire.Todos
	return nil
}

type sessionStore struct {
	workspaceRoot string
	now           func() time.Time
	writeAtomic   func(string, []byte) error
}

type sessionFile struct {
	id       string
	path     string
	snapshot sessionSnapshot
}

func newSessionStore(workspaceRoot string) (*sessionStore, error) {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		var err error
		workspaceRoot, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve workspace root for sessions: %w", err)
		}
	}
	return &sessionStore{
		workspaceRoot: filepath.Clean(workspaceRoot),
		now:           func() time.Time { return time.Now().UTC() },
		writeAtomic:   func(path string, data []byte) error { return atomicWriteFile(path, data, 0o600) },
	}, nil
}

func (s *sessionStore) directory() string {
	return filepath.Join(s.workspaceRoot, sessionStateDir, sessionSnapshotsDir)
}

func (s *sessionStore) create(messages []types.Message) (sessionSnapshot, error) {
	now := s.currentTime()
	snapshot := sessionSnapshot{
		SessionUUID:  generateUUID(),
		CreatedAt:    now,
		UpdatedAt:    now,
		Messages:     cloneMessages(messages),
		Todos:        []todoItem{},
		Topic:        deriveSessionTopic(messages),
		LastInput:    latestDirectUserPrompt(messages),
		MessageCount: len(messages),
	}
	if err := s.save(snapshot); err != nil {
		return sessionSnapshot{}, err
	}
	return snapshot, nil
}

func (s *sessionStore) save(snapshot sessionSnapshot) error {
	id := strings.ToLower(strings.TrimSpace(snapshot.SessionUUID))
	if !isSessionUUID(id) {
		return fmt.Errorf("invalid session UUID %q", snapshot.SessionUUID)
	}
	snapshot.SessionUUID = id
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = s.currentTime()
	}
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = s.currentTime()
	}
	snapshot.Messages = cloneMessages(snapshot.Messages)
	if snapshot.Topic == "" {
		snapshot.Topic = deriveSessionTopic(snapshot.Messages)
	}
	if snapshot.LastInput == "" {
		snapshot.LastInput = latestDirectUserPrompt(snapshot.Messages)
	}
	snapshot.Todos = cloneTodos(snapshot.Todos)
	if err := validateTodos(snapshot.Todos); err != nil {
		return fmt.Errorf("invalid session todos: %w", err)
	}
	snapshot.MessageCount = len(snapshot.Messages)

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %s: %w", id, err)
	}
	if err := os.MkdirAll(s.directory(), 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	if err := s.writeAtomic(s.pathFor(id), data); err != nil {
		return fmt.Errorf("save session %s: %w", id, err)
	}
	return nil
}

// resolve selects a valid snapshot. An empty selector means newest valid
// snapshot; a non-empty selector is an exact ID or a unique ID prefix.
func (s *sessionStore) resolve(selector string) (sessionSnapshot, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector != "" && !isSessionSelector(selector) {
		return sessionSnapshot{}, fmt.Errorf("invalid session ID or prefix %q", selector)
	}

	files, err := s.sessionFiles(selector)
	if err != nil {
		return sessionSnapshot{}, err
	}
	if selector == "" {
		return s.newestValid(files)
	}
	if len(files) == 0 {
		return sessionSnapshot{}, fmt.Errorf("session %q not found", selector)
	}
	if len(files) > 1 {
		ids := make([]string, 0, len(files))
		for _, file := range files {
			ids = append(ids, file.id)
		}
		return sessionSnapshot{}, fmt.Errorf("session prefix %q is ambiguous (%s)", selector, strings.Join(ids, ", "))
	}

	snapshot, err := s.read(files[0])
	if err != nil {
		return sessionSnapshot{}, fmt.Errorf("load session %s: %w", files[0].id, err)
	}
	return snapshot, nil
}

func (s *sessionStore) newestValid(files []sessionFile) (sessionSnapshot, error) {
	valid := make([]sessionFile, 0, len(files))
	for _, file := range files {
		snapshot, err := s.read(file)
		if err != nil {
			// Bare resume is intentionally tolerant of malformed snapshots.
			continue
		}
		file.snapshot = snapshot
		valid = append(valid, file)
	}
	if len(valid) == 0 {
		return sessionSnapshot{}, errors.New("no valid saved sessions found")
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if valid[i].snapshot.UpdatedAt.Equal(valid[j].snapshot.UpdatedAt) {
			return valid[i].id > valid[j].id
		}
		return valid[i].snapshot.UpdatedAt.After(valid[j].snapshot.UpdatedAt)
	})
	return valid[0].snapshot, nil
}

func (s *sessionStore) sessionFiles(selector string) ([]sessionFile, error) {
	entries, err := os.ReadDir(s.directory())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session directory: %w", err)
	}

	files := make([]sessionFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != sessionSnapshotSuffix {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), sessionSnapshotSuffix)
		if !isSessionUUID(id) {
			// Never treat arbitrary filenames as session selectors or paths.
			continue
		}
		id = strings.ToLower(id)
		if selector != "" && !strings.HasPrefix(id, selector) {
			continue
		}
		files = append(files, sessionFile{id: id, path: filepath.Join(s.directory(), entry.Name())})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })
	return files, nil
}

func (s *sessionStore) read(file sessionFile) (sessionSnapshot, error) {
	data, err := os.ReadFile(file.path)
	if err != nil {
		return sessionSnapshot{}, err
	}
	var snapshot sessionSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return sessionSnapshot{}, fmt.Errorf("decode JSON: %w", err)
	}
	if snapshot.SessionUUID == "" {
		// The filename is the authoritative identity, allowing snapshots from
		// the earliest persistence format to remain loadable.
		snapshot.SessionUUID = file.id
	}
	snapshot.SessionUUID = strings.ToLower(strings.TrimSpace(snapshot.SessionUUID))
	if snapshot.SessionUUID != file.id {
		return sessionSnapshot{}, fmt.Errorf("snapshot ID %q does not match filename", snapshot.SessionUUID)
	}
	if !isSessionUUID(snapshot.SessionUUID) {
		return sessionSnapshot{}, fmt.Errorf("invalid snapshot UUID %q", snapshot.SessionUUID)
	}
	if snapshot.Messages == nil {
		return sessionSnapshot{}, errors.New("snapshot has no messages")
	}
	snapshot.Messages = cloneMessages(snapshot.Messages)
	if snapshot.Todos == nil {
		snapshot.Todos = []todoItem{}
	} else {
		snapshot.Todos = cloneTodos(snapshot.Todos)
	}
	if snapshot.Topic == "" {
		snapshot.Topic = deriveSessionTopic(snapshot.Messages)
	}
	if snapshot.LastInput == "" {
		snapshot.LastInput = latestDirectUserPrompt(snapshot.Messages)
	}
	if err := validateTodos(snapshot.Todos); err != nil {
		return sessionSnapshot{}, fmt.Errorf("invalid snapshot todos: %w", err)
	}
	snapshot.MessageCount = len(snapshot.Messages)
	return snapshot, nil
}

func (s *sessionStore) list() ([]sessionSnapshot, error) {
	files, err := s.sessionFiles("")
	if err != nil {
		return nil, err
	}
	valid := make([]sessionSnapshot, 0, len(files))
	for _, file := range files {
		snapshot, err := s.read(file)
		if err != nil {
			continue
		}
		valid = append(valid, snapshot)
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if valid[i].UpdatedAt.Equal(valid[j].UpdatedAt) {
			return valid[i].SessionUUID > valid[j].SessionUUID
		}
		return valid[i].UpdatedAt.After(valid[j].UpdatedAt)
	})
	return valid, nil
}

func (s *sessionStore) pathFor(id string) string {
	return filepath.Join(s.directory(), id+sessionSnapshotSuffix)
}

func (s *sessionStore) currentTime() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func isSessionUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

func isSessionSelector(value string) bool {
	if value == "" || len(value) > 36 {
		return false
	}
	for _, r := range value {
		if !isHexDigit(r) && r != '-' {
			return false
		}
	}
	return true
}

func isHexDigit(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

func atomicWriteFile(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

func cloneMessages(messages []types.Message) []types.Message {
	if messages == nil {
		return nil
	}
	clone := make([]types.Message, len(messages))
	copy(clone, messages)
	for i := range clone {
		if messages[i].ToolCalls != nil {
			clone[i].ToolCalls = append([]types.ToolCall(nil), messages[i].ToolCalls...)
		}
	}
	return clone
}
