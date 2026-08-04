// Package sessions owns durable interactive conversation snapshots and the
// filesystem rules used to create, replace, enumerate, and resume them.
package sessions

import (
	"capelin-go/internal/contracts"
	"crypto/rand"
	"encoding/hex"
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
	stateDir       = ".capelin-go"
	snapshotsDir   = "sessions"
	snapshotSuffix = ".json"
)

// Todo is the durable checklist item associated with a session. It is kept in
// this package so session persistence does not depend on the application or
// tool implementation packages.
type Todo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Source  string `json:"source,omitempty"`
	Status  string `json:"status"`
}

// Snapshot is the stable, provider-neutral representation of an interactive
// conversation. Optional metadata is deliberately retained for old snapshots.
type Snapshot struct {
	SessionUUID   string                       `json:"sessionUUID"`
	CreatedAt     time.Time                    `json:"createdAt"`
	UpdatedAt     time.Time                    `json:"updatedAt"`
	LastContent   string                       `json:"lastContent,omitempty"`
	Name          string                       `json:"name,omitempty"`
	Topic         string                       `json:"topic,omitempty"`
	LastInput     string                       `json:"lastInput,omitempty"`
	MessageCount  int                          `json:"messageCount"`
	Messages      []contracts.Message          `json:"messages"`
	Todos         []Todo                       `json:"todos"`
	ProviderState *contracts.ContinuationState `json:"providerState,omitempty"`
}

// UnmarshalJSON accepts the original camelCase representation and aliases
// emitted by early development versions.
func (s *Snapshot) UnmarshalJSON(data []byte) error {
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
		Todos                []Todo              `json:"todos"`
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

// Store is a concrete filesystem-backed session store. The two setters are
// intentional test seams: production uses the defaults, while tests can
// inject a clock and an atomic writer without touching the filesystem globally.
type Store struct {
	workspaceRoot string
	now           func() time.Time
	writeAtomic   func(string, []byte) error
}

func New(workspaceRoot string) (*Store, error) {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		var err error
		workspaceRoot, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve workspace root for sessions: %w", err)
		}
	}
	return &Store{
		workspaceRoot: filepath.Clean(workspaceRoot),
		now:           func() time.Time { return time.Now().UTC() },
		writeAtomic:   func(path string, data []byte) error { return atomicWriteFile(path, data, 0o600) },
	}, nil
}

func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Store) SetAtomicWriter(writer func(string, []byte) error) {
	if writer != nil {
		s.writeAtomic = writer
	}
}

func (s *Store) Directory() string { return filepath.Join(s.workspaceRoot, stateDir, snapshotsDir) }

func (s *Store) Create(messages []contracts.Message) (Snapshot, error) {
	now := s.currentTime()
	snapshot := Snapshot{
		SessionUUID: generateUUID(), CreatedAt: now, UpdatedAt: now,
		Messages: cloneMessages(messages), Todos: []Todo{},
		Topic: deriveTopic(messages), LastInput: latestPrompt(messages), MessageCount: len(messages),
	}
	if err := s.Save(snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) Save(snapshot Snapshot) error {
	id := strings.ToLower(strings.TrimSpace(snapshot.SessionUUID))
	if !IsUUID(id) {
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
	snapshot.ProviderState = cloneProviderState(snapshot.ProviderState)
	if snapshot.Topic == "" {
		snapshot.Topic = deriveTopic(snapshot.Messages)
	}
	if snapshot.LastInput == "" {
		snapshot.LastInput = latestPrompt(snapshot.Messages)
	}
	snapshot.Todos = cloneTodos(snapshot.Todos)
	if err := ValidateTodos(snapshot.Todos); err != nil {
		return fmt.Errorf("invalid session todos: %w", err)
	}
	snapshot.MessageCount = len(snapshot.Messages)
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %s: %w", id, err)
	}
	if err := os.MkdirAll(s.Directory(), 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	writer := s.writeAtomic
	if writer == nil {
		writer = func(path string, data []byte) error { return atomicWriteFile(path, data, 0o600) }
	}
	if err := writer(s.pathFor(id), data); err != nil {
		return fmt.Errorf("save session %s: %w", id, err)
	}
	return nil
}

// Resolve selects an exact ID, a unique ID prefix, or the newest valid
// snapshot when selector is empty. Bare resume tolerates malformed snapshots.
func (s *Store) Resolve(selector string) (Snapshot, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector != "" && !IsSelector(selector) {
		return Snapshot{}, fmt.Errorf("invalid session ID or prefix %q", selector)
	}
	files, err := s.sessionFiles(selector)
	if err != nil {
		return Snapshot{}, err
	}
	if selector == "" {
		return s.newestValid(files)
	}
	if len(files) == 0 {
		return Snapshot{}, fmt.Errorf("session %q not found", selector)
	}
	if len(files) > 1 {
		ids := make([]string, 0, len(files))
		for _, f := range files {
			ids = append(ids, f.id)
		}
		return Snapshot{}, fmt.Errorf("session prefix %q is ambiguous (%s)", selector, strings.Join(ids, ", "))
	}
	snapshot, err := s.read(files[0])
	if err != nil {
		return Snapshot{}, fmt.Errorf("load session %s: %w", files[0].id, err)
	}
	return snapshot, nil
}

func (s *Store) List() ([]Snapshot, error) {
	files, err := s.sessionFiles("")
	if err != nil {
		return nil, err
	}
	valid := make([]Snapshot, 0, len(files))
	for _, file := range files {
		snapshot, err := s.read(file)
		if err == nil {
			valid = append(valid, snapshot)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if valid[i].UpdatedAt.Equal(valid[j].UpdatedAt) {
			return valid[i].SessionUUID > valid[j].SessionUUID
		}
		return valid[i].UpdatedAt.After(valid[j].UpdatedAt)
	})
	return valid, nil
}

type sessionFile struct{ id, path string }

func (s *Store) newestValid(files []sessionFile) (Snapshot, error) {
	valid := make([]struct {
		id       string
		snapshot Snapshot
	}, 0, len(files))
	for _, file := range files {
		snapshot, err := s.read(file)
		if err == nil {
			valid = append(valid, struct {
				id       string
				snapshot Snapshot
			}{file.id, snapshot})
		}
	}
	if len(valid) == 0 {
		return Snapshot{}, errors.New("no valid saved sessions found")
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if valid[i].snapshot.UpdatedAt.Equal(valid[j].snapshot.UpdatedAt) {
			return valid[i].id > valid[j].id
		}
		return valid[i].snapshot.UpdatedAt.After(valid[j].snapshot.UpdatedAt)
	})
	return valid[0].snapshot, nil
}

func (s *Store) sessionFiles(selector string) ([]sessionFile, error) {
	entries, err := os.ReadDir(s.Directory())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session directory: %w", err)
	}
	files := make([]sessionFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != snapshotSuffix {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), snapshotSuffix)
		if !IsUUID(id) {
			continue
		}
		id = strings.ToLower(id)
		if selector != "" && !strings.HasPrefix(id, selector) {
			continue
		}
		files = append(files, sessionFile{id: id, path: filepath.Join(s.Directory(), entry.Name())})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })
	return files, nil
}

func (s *Store) read(file sessionFile) (Snapshot, error) {
	data, err := os.ReadFile(file.path)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode JSON: %w", err)
	}
	if snapshot.SessionUUID == "" {
		snapshot.SessionUUID = file.id
	}
	snapshot.SessionUUID = strings.ToLower(strings.TrimSpace(snapshot.SessionUUID))
	if snapshot.SessionUUID != file.id {
		return Snapshot{}, fmt.Errorf("snapshot ID %q does not match filename", snapshot.SessionUUID)
	}
	if !IsUUID(snapshot.SessionUUID) {
		return Snapshot{}, fmt.Errorf("invalid snapshot UUID %q", snapshot.SessionUUID)
	}
	if snapshot.Messages == nil {
		return Snapshot{}, errors.New("snapshot has no messages")
	}
	snapshot.Messages = cloneMessages(snapshot.Messages)
	if snapshot.Todos == nil {
		snapshot.Todos = []Todo{}
	} else {
		snapshot.Todos = cloneTodos(snapshot.Todos)
	}
	if snapshot.Topic == "" {
		snapshot.Topic = deriveTopic(snapshot.Messages)
	}
	if snapshot.LastInput == "" {
		snapshot.LastInput = latestPrompt(snapshot.Messages)
	}
	if err := ValidateTodos(snapshot.Todos); err != nil {
		return Snapshot{}, fmt.Errorf("invalid snapshot todos: %w", err)
	}
	snapshot.MessageCount = len(snapshot.Messages)
	return snapshot, nil
}

// ValidateTodos enforces the same durable checklist contract as the tool
// boundary. Unknown statuses and duplicate/empty IDs are rejected.
func ValidateTodos(todos []Todo) error {
	seen := make(map[string]struct{}, len(todos))
	for _, todo := range todos {
		if strings.TrimSpace(todo.ID) == "" {
			return errors.New("todo ID is required")
		}
		if strings.TrimSpace(todo.Content) == "" {
			return fmt.Errorf("todo %q content is required", todo.ID)
		}
		if _, ok := seen[todo.ID]; ok {
			return fmt.Errorf("duplicate todo ID %q", todo.ID)
		}
		seen[todo.ID] = struct{}{}
		switch todo.Status {
		case "pending", "in_progress", "completed", "cancelled":
		default:
			return fmt.Errorf("todo %q has unsupported status %q", todo.ID, todo.Status)
		}
	}
	return nil
}

func (s *Store) pathFor(id string) string { return filepath.Join(s.Directory(), id+snapshotSuffix) }
func (s *Store) currentTime() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func IsUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !isHex(r) {
			return false
		}
	}
	return true
}
func IsSelector(value string) bool {
	if value == "" || len(value) > 36 {
		return false
	}
	for _, r := range value {
		if !isHex(r) && r != '-' {
			return false
		}
	}
	return true
}
func isHex(r rune) bool { return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' }

func deriveTopic(messages []contracts.Message) string {
	for _, message := range messages {
		if message.Role == "user" && directPrompt(message.Content) {
			return strings.TrimSpace(message.Content)
		}
	}
	return ""
}
func latestPrompt(messages []contracts.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && directPrompt(messages[i].Content) {
			return strings.TrimSpace(messages[i].Content)
		}
	}
	return ""
}
func directPrompt(content string) bool {
	content = strings.TrimSpace(content)
	return content != "" && !strings.HasPrefix(content, "Start working toward this objective:") && !strings.HasPrefix(content, "Continue working toward the objective.") && !strings.HasPrefix(content, "[SYSTEM]")
}
func cloneMessages(messages []contracts.Message) []contracts.Message {
	if messages == nil {
		return nil
	}
	clone := make([]contracts.Message, len(messages))
	copy(clone, messages)
	for i := range clone {
		clone[i].ToolCalls = append([]contracts.ToolCall(nil), messages[i].ToolCalls...)
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
func cloneTodos(todos []Todo) []Todo {
	if todos == nil {
		return nil
	}
	clone := make([]Todo, len(todos))
	copy(clone, todos)
	return clone
}

func generateUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func atomicWriteFile(path string, data []byte, mode fs.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	remove := true
	defer func() {
		if remove {
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
	remove = false
	return nil
}
