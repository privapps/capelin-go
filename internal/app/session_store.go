package app

import (
	"capelin-go/internal/contracts"
	sessionpkg "capelin-go/internal/sessions"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	sessionStateDir       = ".capelin-go"
	sessionSnapshotsDir   = "sessions"
	sessionSnapshotSuffix = ".json"
)

// sessionSnapshot remains the application compatibility shape. Durable I/O is
// owned by internal/sessions; this local type keeps existing app tests and
// callers independent from the concrete filesystem package.
type sessionSnapshot struct {
	SessionUUID   string                       `json:"sessionUUID"`
	CreatedAt     time.Time                    `json:"createdAt"`
	UpdatedAt     time.Time                    `json:"updatedAt"`
	LastContent   string                       `json:"lastContent,omitempty"`
	Name          string                       `json:"name,omitempty"`
	Topic         string                       `json:"topic,omitempty"`
	LastInput     string                       `json:"lastInput,omitempty"`
	MessageCount  int                          `json:"messageCount"`
	Messages      []contracts.Message          `json:"messages"`
	Todos         []todoItem                   `json:"todos"`
	ActiveGoal    *goalState                   `json:"activeGoal,omitempty"`
	ProviderState *contracts.ContinuationState `json:"providerState,omitempty"`
}

// UnmarshalJSON accepts legacy aliases for callers that still decode the
// application compatibility type directly. The sessions module performs the
// same compatibility handling at the filesystem boundary.
func (s *sessionSnapshot) UnmarshalJSON(data []byte) error {
	var wire struct {
		SessionUUID          string              `json:"sessionUUID"`
		SessionID            string              `json:"session_id"`
		ID                   string              `json:"id"`
		CreatedAt            time.Time           `json:"createdAt"`
		CreatedAtSnake       time.Time           `json:"created_at"`
		UpdatedAt            time.Time           `json:"updatedAt"`
		UpdatedAtSnake       time.Time           `json:"updated_at"`
		LastContent          string              `json:"lastContent"`
		LastResponse         string              `json:"last_response"`
		Name                 string              `json:"name"`
		Topic                string              `json:"topic"`
		LastInput            string              `json:"lastInput"`
		LastInputSnake       string              `json:"last_input"`
		MessageCount         int                 `json:"messageCount"`
		Messages             []contracts.Message `json:"messages"`
		Todos                []todoItem          `json:"todos"`
		ActiveGoal           *goalState          `json:"activeGoal"`
		ProviderStateRaw     json.RawMessage     `json:"providerState"`
		ContinuationStateRaw json.RawMessage     `json:"continuationState"`
		ProviderStateSnake   json.RawMessage     `json:"provider_state"`
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
	s.ActiveGoal = cloneGoalState(wire.ActiveGoal)
	stateRaw := wire.ProviderStateRaw
	if len(stateRaw) == 0 {
		stateRaw = wire.ContinuationStateRaw
	}
	if len(stateRaw) == 0 {
		stateRaw = wire.ProviderStateSnake
	}
	if len(stateRaw) > 0 && string(stateRaw) != "null" {
		var state contracts.ContinuationState
		if json.Unmarshal(stateRaw, &state) == nil {
			s.ProviderState = &state
		}
	}
	return nil
}

type sessionStore struct {
	workspaceRoot string
	now           func() time.Time
	writeAtomic   func(string, []byte) error
	backend       *sessionpkg.Store
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
	backend, err := sessionpkg.New(workspaceRoot)
	if err != nil {
		return nil, err
	}
	return &sessionStore{
		workspaceRoot: filepath.Clean(strings.TrimSpace(workspaceRoot)),
		now:           func() time.Time { return time.Now().UTC() },
		writeAtomic:   func(path string, data []byte) error { return atomicWriteFile(path, data, 0o600) },
		backend:       backend,
	}, nil
}

func (s *sessionStore) syncDependencies() {
	if s.backend == nil {
		backend, err := sessionpkg.New(s.workspaceRoot)
		if err == nil {
			s.backend = backend
		}
	}
	if s.backend == nil {
		return
	}
	s.backend.SetClock(s.now)
	s.backend.SetAtomicWriter(s.writeAtomic)
}

func (s *sessionStore) directory() string {
	if s.backend != nil {
		return s.backend.Directory()
	}
	return filepath.Join(s.workspaceRoot, sessionStateDir, sessionSnapshotsDir)
}

func (s *sessionStore) create(messages []contracts.Message) (sessionSnapshot, error) {
	s.syncDependencies()
	if s.backend == nil {
		return sessionSnapshot{}, errors.New("session store is unavailable")
	}
	snapshot, err := s.backend.Create(toContractsMessages(messages))
	if err != nil {
		return sessionSnapshot{}, err
	}
	return fromSessionSnapshot(snapshot), nil
}

func (s *sessionStore) save(snapshot sessionSnapshot) error {
	s.syncDependencies()
	if s.backend == nil {
		return errors.New("session store is unavailable")
	}
	return s.backend.Save(toSessionSnapshot(snapshot))
}

func (s *sessionStore) resolve(selector string) (sessionSnapshot, error) {
	s.syncDependencies()
	if s.backend == nil {
		return sessionSnapshot{}, errors.New("session store is unavailable")
	}
	snapshot, err := s.backend.Resolve(selector)
	if err != nil {
		return sessionSnapshot{}, err
	}
	return fromSessionSnapshot(snapshot), nil
}

func (s *sessionStore) list() ([]sessionSnapshot, error) {
	s.syncDependencies()
	if s.backend == nil {
		return nil, errors.New("session store is unavailable")
	}
	snapshots, err := s.backend.List()
	if err != nil {
		return nil, err
	}
	result := make([]sessionSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		result = append(result, fromSessionSnapshot(snapshot))
	}
	return result, nil
}

func (s *sessionStore) currentTime() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *sessionStore) pathFor(id string) string {
	return filepath.Join(s.directory(), id+sessionSnapshotSuffix)
}

func cloneMessages(messages []contracts.Message) []contracts.Message {
	if messages == nil {
		return nil
	}
	clone := make([]contracts.Message, len(messages))
	copy(clone, messages)
	for i := range clone {
		if messages[i].ToolCalls != nil {
			clone[i].ToolCalls = append([]contracts.ToolCall(nil), messages[i].ToolCalls...)
		}
		if messages[i].ReasoningContent != nil {
			value := *messages[i].ReasoningContent
			clone[i].ReasoningContent = &value
		}
	}
	return clone
}

func cloneProviderState(state *contracts.ContinuationState) *contracts.ContinuationState {
	if state == nil {
		return nil
	}
	clone := *state
	clone.Data = append(json.RawMessage(nil), state.Data...)
	return &clone
}

func toContractsMessages(messages []contracts.Message) []contracts.Message {
	return cloneMessages(messages)
}

func toSessionSnapshot(snapshot sessionSnapshot) sessionpkg.Snapshot {
	todos := make([]sessionpkg.Todo, 0, len(snapshot.Todos))
	for _, todo := range snapshot.Todos {
		todos = append(todos, sessionpkg.Todo{ID: todo.ID, Content: todo.Content, Source: todo.Source, Status: string(todo.Status)})
	}
	return sessionpkg.Snapshot{
		SessionUUID: snapshot.SessionUUID, CreatedAt: snapshot.CreatedAt, UpdatedAt: snapshot.UpdatedAt,
		LastContent: snapshot.LastContent, Name: snapshot.Name, Topic: snapshot.Topic, LastInput: snapshot.LastInput,
		MessageCount: snapshot.MessageCount, Messages: toContractsMessages(snapshot.Messages), Todos: todos,
		ActiveGoal:    toSessionGoal(snapshot.ActiveGoal),
		ProviderState: cloneProviderState(snapshot.ProviderState),
	}
}

func fromSessionSnapshot(snapshot sessionpkg.Snapshot) sessionSnapshot {
	todos := make([]todoItem, 0, len(snapshot.Todos))
	for _, todo := range snapshot.Todos {
		todos = append(todos, todoItem{ID: todo.ID, Content: todo.Content, Source: todo.Source, Status: todoStatus(todo.Status)})
	}
	return sessionSnapshot{
		SessionUUID: snapshot.SessionUUID, CreatedAt: snapshot.CreatedAt, UpdatedAt: snapshot.UpdatedAt,
		LastContent: snapshot.LastContent, Name: snapshot.Name, Topic: snapshot.Topic, LastInput: snapshot.LastInput,
		MessageCount: snapshot.MessageCount, Messages: cloneMessages(snapshot.Messages), Todos: todos,
		ActiveGoal:    fromSessionGoal(snapshot.ActiveGoal),
		ProviderState: cloneProviderState(snapshot.ProviderState),
	}
}

func isSessionUUID(value string) bool     { return sessionpkg.IsUUID(value) }
func isSessionSelector(value string) bool { return sessionpkg.IsSelector(value) }
func isHexDigit(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

// Kept as a small compatibility helper for tests and callers that exercise the
// atomic replacement primitive directly. Production session writes use the
// equivalent implementation in internal/sessions.
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
