package app

import (
	"capelin-go/internal/contracts"
	sessionpkg "capelin-go/internal/sessions"
	"errors"
	"strings"
	"time"
)

// sessionView is an application workflow view of a durable session. It has no
// persistence tags or decoding behavior: internal/sessions owns the durable
// Snapshot and all of its compatibility rules. The view only adapts checklist
// and goal values to the application workflow types.
type sessionView struct {
	SessionUUID   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastContent   string
	Name          string
	Topic         string
	LastInput     string
	MessageCount  int
	Messages      []contracts.Message
	Todos         []todoItem
	ActiveGoal    *goalState
	ProviderState *contracts.ContinuationState
}

// sessionStore is a narrow application seam over the canonical sessions store.
// Its clock and atomic writer are dependency injection hooks only; filesystem
// layout, encoding, validation, and replacement remain in internal/sessions.
type sessionStore struct {
	now         func() time.Time
	writeAtomic func(string, []byte) error
	backend     *sessionpkg.Store
}

func newSessionStore(workspaceRoot string) (*sessionStore, error) {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	backend, err := sessionpkg.New(workspaceRoot)
	if err != nil {
		return nil, err
	}
	return &sessionStore{
		now:     func() time.Time { return time.Now().UTC() },
		backend: backend,
	}, nil
}

func (s *sessionStore) syncDependencies() {
	if s == nil || s.backend == nil {
		return
	}
	if s.now != nil {
		s.backend.SetClock(s.now)
	}
	if s.writeAtomic != nil {
		s.backend.SetAtomicWriter(s.writeAtomic)
	}
}

func (s *sessionStore) directory() string {
	if s == nil || s.backend == nil {
		return ""
	}
	return s.backend.Directory()
}

func (s *sessionStore) create(messages []contracts.Message) (sessionView, error) {
	s.syncDependencies()
	if s == nil || s.backend == nil {
		return sessionView{}, errors.New("session store is unavailable")
	}
	snapshot, err := s.backend.Create(messages)
	if err != nil {
		return sessionView{}, err
	}
	return fromDurableSnapshot(snapshot), nil
}

func (s *sessionStore) save(snapshot sessionView) error {
	s.syncDependencies()
	if s == nil || s.backend == nil {
		return errors.New("session store is unavailable")
	}
	return s.backend.Save(toDurableSnapshot(snapshot))
}

func (s *sessionStore) resolve(selector string) (sessionView, error) {
	s.syncDependencies()
	if s == nil || s.backend == nil {
		return sessionView{}, errors.New("session store is unavailable")
	}
	snapshot, err := s.backend.Resolve(selector)
	if err != nil {
		return sessionView{}, err
	}
	return fromDurableSnapshot(snapshot), nil
}

func (s *sessionStore) list() ([]sessionView, error) {
	s.syncDependencies()
	if s == nil || s.backend == nil {
		return nil, errors.New("session store is unavailable")
	}
	snapshots, err := s.backend.List()
	if err != nil {
		return nil, err
	}
	result := make([]sessionView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		result = append(result, fromDurableSnapshot(snapshot))
	}
	return result, nil
}

func (s *sessionStore) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

// pathFor is retained only for package-local tests that inspect the canonical
// file produced by the sessions backend. The path construction itself belongs
// to internal/sessions.
func (s *sessionStore) pathFor(id string) string {
	if s == nil || s.backend == nil {
		return ""
	}
	return s.backend.Path(id)
}

func toDurableSnapshot(view sessionView) sessionpkg.Snapshot {
	todos := make([]sessionpkg.Todo, 0, len(view.Todos))
	for _, todo := range view.Todos {
		todos = append(todos, sessionpkg.Todo{
			ID: todo.ID, Content: todo.Content, Source: todo.Source, Status: string(todo.Status),
		})
	}
	return sessionpkg.Snapshot{
		SessionUUID:   view.SessionUUID,
		CreatedAt:     view.CreatedAt,
		UpdatedAt:     view.UpdatedAt,
		LastContent:   view.LastContent,
		Name:          view.Name,
		Topic:         view.Topic,
		LastInput:     view.LastInput,
		MessageCount:  view.MessageCount,
		Messages:      cloneMessages(view.Messages),
		Todos:         todos,
		ActiveGoal:    toSessionGoal(view.ActiveGoal),
		ProviderState: cloneProviderState(view.ProviderState),
	}
}

func fromDurableSnapshot(snapshot sessionpkg.Snapshot) sessionView {
	todos := make([]todoItem, 0, len(snapshot.Todos))
	for _, todo := range snapshot.Todos {
		todos = append(todos, todoItem{
			ID: todo.ID, Content: todo.Content, Source: todo.Source, Status: todoStatus(todo.Status),
		})
	}
	return sessionView{
		SessionUUID:   snapshot.SessionUUID,
		CreatedAt:     snapshot.CreatedAt,
		UpdatedAt:     snapshot.UpdatedAt,
		LastContent:   snapshot.LastContent,
		Name:          snapshot.Name,
		Topic:         snapshot.Topic,
		LastInput:     snapshot.LastInput,
		MessageCount:  snapshot.MessageCount,
		Messages:      cloneMessages(snapshot.Messages),
		Todos:         todos,
		ActiveGoal:    fromSessionGoal(snapshot.ActiveGoal),
		ProviderState: cloneProviderState(snapshot.ProviderState),
	}
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
	clone.Data = append([]byte(nil), state.Data...)
	return &clone
}
