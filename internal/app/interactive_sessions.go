package app

import (
	"capelin-go/internal/contracts"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func interactiveCommandArgument(input, command string) (string, bool) {
	if input == command {
		return "", true
	}
	if strings.HasPrefix(input, command+" ") {
		return strings.TrimSpace(strings.TrimPrefix(input, command)), true
	}
	return "", false
}

func (a *app) writeInteractiveSystem(message string) {
	if a != nil && a.sink != nil {
		a.sink.WriteSystem(rootAgentID, message)
		return
	}
	fmt.Fprintln(os.Stderr, message)
}

func (a *app) ensureSessionStore() (*sessionStore, error) {
	if a.sessionStore != nil {
		return a.sessionStore, nil
	}
	store, err := newSessionStore(a.cfg.workspaceRoot)
	if err != nil {
		return nil, err
	}
	a.sessionStore = store
	return store, nil
}

func (a *app) startInteractiveSession() (*interactiveSession, error) {
	store, err := a.ensureSessionStore()
	if err != nil {
		return nil, err
	}
	if a.cfg.resumeRequested || a.cfg.resumeID != "" {
		snapshot, err := store.resolve(a.cfg.resumeID)
		if err != nil {
			return nil, err
		}
		return a.sessionFromSnapshot(snapshot), nil
	}
	return a.newInteractiveSession([]contracts.Message{{Role: "system", Content: a.systemPromptWithSkills()}})
}

func (a *app) initializeInteractiveSession(session *interactiveSession) error {
	if session == nil {
		return fmt.Errorf("interactive session is nil")
	}
	if session.id == "" {
		if session.messages == nil {
			session.messages = []contracts.Message{{Role: "system", Content: a.systemPromptWithSkills()}}
		}
		store, err := a.ensureSessionStore()
		if err != nil {
			return err
		}
		snapshot, err := store.create(session.messages)
		if err != nil {
			return err
		}
		session.id = snapshot.SessionUUID
		session.createdAt = snapshot.CreatedAt
		if session.loadedSkills == nil {
			session.loadedSkills = make(map[string]bool)
		}
		if session.runtime == nil {
			session.runtime = a.rootRuntime()
		}
		session.todos = cloneTodos(snapshot.Todos)
	}
	a.attachInteractiveRuntime(session)
	a.attachInteractiveSaver(session)
	return nil
}

func (a *app) newInteractiveSession(messages []contracts.Message) (*interactiveSession, error) {
	store, err := a.ensureSessionStore()
	if err != nil {
		return nil, err
	}
	snapshot, err := store.create(messages)
	if err != nil {
		return nil, err
	}
	return a.sessionFromSnapshot(snapshot), nil
}

func (a *app) sessionFromSnapshot(snapshot sessionSnapshot) *interactiveSession {
	session := &interactiveSession{
		messages:      cloneMessages(snapshot.Messages),
		providerState: cloneProviderState(snapshot.ProviderState),
		lastResponse:  snapshot.LastContent,
		loadedSkills:  make(map[string]bool),
		id:            snapshot.SessionUUID,
		createdAt:     snapshot.CreatedAt,
		todos:         cloneTodos(snapshot.Todos),
		name:          snapshot.Name,
		topic:         snapshot.Topic,
		lastInput:     snapshot.LastInput,
	}
	session.runtime = a.rootRuntime()
	a.attachInteractiveRuntime(session)
	a.attachInteractiveSaver(session)
	return session
}

func (a *app) attachInteractiveRuntime(session *interactiveSession) {
	if session == nil {
		return
	}
	if session.runtime == nil {
		session.runtime = a.rootRuntime()
	}
	session.runtime.todosMu.Lock()
	session.runtime.todos = cloneTodos(session.todos)
	session.runtime.todosMu.Unlock()
	session.runtime.todosChanged = func(todos []todoItem) {
		session.todos = cloneTodos(todos)
		if err := a.saveInteractiveSession(session); err != nil {
			session.runtime.recordFatalError(err)
			fmt.Fprintf(os.Stderr, "[capelin-go] warning: could not save checklist: %v\n", err)
		}
	}
}

func (a *app) attachInteractiveSaver(session *interactiveSession) {
	if session != nil {
		session.save = func() error { return a.saveInteractiveSession(session) }
	}
}

func (a *app) saveInteractiveSession(session *interactiveSession) error {
	if session == nil || strings.TrimSpace(session.id) == "" {
		return nil
	}
	store, err := a.ensureSessionStore()
	if err != nil {
		return newSessionPersistenceError(err)
	}
	created := session.createdAt
	if created.IsZero() {
		created = store.currentTime()
		session.createdAt = created
	}
	snapshot := sessionSnapshot{
		SessionUUID:   session.id,
		CreatedAt:     created,
		UpdatedAt:     store.currentTime(),
		LastContent:   session.lastResponse,
		Name:          strings.TrimSpace(session.name),
		Topic:         session.topic,
		LastInput:     session.lastInput,
		Messages:      cloneMessages(session.messages),
		Todos:         cloneTodos(session.todos),
		ProviderState: cloneProviderState(session.providerState),
	}
	if err := store.save(snapshot); err != nil {
		return newSessionPersistenceError(err)
	}
	return nil
}

func (a *app) finishInteractiveSession(session *interactiveSession) {
	if session == nil || strings.TrimSpace(session.id) == "" {
		return
	}
	if err := a.saveInteractiveSession(session); err != nil {
		fmt.Fprintf(os.Stderr, "[capelin-go] warning: could not save session %s: %v\n", session.id, err)
	}
	fmt.Fprintf(os.Stderr, "[capelin-go] session %s; resume with --resume %s\n", session.id, session.id)
}

func (a *app) switchToNewSession(session *interactiveSession) error {
	if session == nil {
		return errors.New("interactive session is nil")
	}
	if err := a.saveInteractiveSession(session); err != nil {
		return err
	}
	next, err := a.newInteractiveSession([]contracts.Message{{Role: "system", Content: a.systemPromptWithSkills()}})
	if err != nil {
		return err
	}
	*session = *next
	a.attachInteractiveRuntime(session)
	a.attachInteractiveSaver(session)
	return nil
}

func (a *app) switchToSavedSession(session *interactiveSession, selector string) error {
	if session == nil {
		return errors.New("interactive session is nil")
	}
	store, err := a.ensureSessionStore()
	if err != nil {
		return err
	}
	snapshot, err := store.resolve(selector)
	if err != nil {
		return err
	}
	if err := a.saveInteractiveSession(session); err != nil {
		return fmt.Errorf("save current session before resume: %w", err)
	}
	next := a.sessionFromSnapshot(snapshot)
	*session = *next
	a.attachInteractiveRuntime(session)
	a.attachInteractiveSaver(session)
	return nil
}

func (a *app) listInteractiveSessions(current *interactiveSession) error {
	store, err := a.ensureSessionStore()
	if err != nil {
		return err
	}
	snapshots, err := store.list()
	if err != nil {
		return err
	}
	if len(snapshots) == 0 {
		fmt.Fprintln(os.Stderr, "[capelin-go] no saved sessions")
		return nil
	}
	for _, snapshot := range snapshots {
		marker := " "
		if current != nil && snapshot.SessionUUID == current.id {
			marker = "*"
		}
		fmt.Fprintf(os.Stderr, "%s %s | %s | last: %s | messages: %d | updated: %s%s\n", marker, snapshot.SessionUUID, sessionDisplayLabel(snapshot), displaySessionText(snapshot.LastInput), snapshot.MessageCount, snapshot.UpdatedAt.Format(time.RFC3339), displayTodoProgress(snapshot.Todos))
	}
	return nil
}

func (a *app) renameInteractiveSession(session *interactiveSession, rawName string) error {
	if session == nil {
		return errors.New("interactive session is nil")
	}
	name := strings.TrimSpace(rawName)
	if name == "--clear" {
		session.name = ""
	} else if name == "" {
		return errors.New("session name is required (or use --clear)")
	} else {
		session.name = name
	}
	if err := a.saveInteractiveSession(session); err != nil {
		return err
	}
	return nil
}

func sessionDisplayLabel(snapshot sessionSnapshot) string {
	if name := strings.TrimSpace(snapshot.Name); name != "" {
		return displaySessionText(name)
	}
	if topic := strings.TrimSpace(snapshot.Topic); topic != "" {
		return displaySessionText(topic)
	}
	return "New session"
}

func displaySessionText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	const max = 72
	if len([]rune(value)) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max-3]) + "..."
}

func displayTodoProgress(todos []todoItem) string {
	if len(todos) == 0 {
		return ""
	}
	completed := 0
	for _, todo := range todos {
		if todo.Status == todoStatusCompleted {
			completed++
		}
	}
	return fmt.Sprintf(" | todos: %d/%d completed", completed, len(todos))
}

func deriveSessionTopic(messages []contracts.Message) string {
	for _, message := range messages {
		if message.Role == "user" && isDirectInteractivePrompt(message.Content) {
			return strings.TrimSpace(message.Content)
		}
	}
	return ""
}

func latestDirectUserPrompt(messages []contracts.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && isDirectInteractivePrompt(messages[i].Content) {
			return strings.TrimSpace(messages[i].Content)
		}
	}
	return ""
}

func isDirectInteractivePrompt(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	return !strings.HasPrefix(content, "Start working toward this objective:") &&
		!strings.HasPrefix(content, "Continue working toward the objective.") &&
		!strings.HasPrefix(content, "[SYSTEM]")
}

func (a *app) runGoal(ctx context.Context, session *interactiveSession, objective string) bool {
	if session == nil {
		a.writeInteractiveSystem("[goal] incomplete: interactive session is nil")
		return false
	}
	if !a.cfg.yolo {
		a.writeInteractiveSystem("[goal] --yolo is required before using /goal")
		return false
	}
	if a.cfg.maxGoalIterations <= 0 {
		a.writeInteractiveSystem("[goal] invalid goal iteration limit")
		return false
	}
	objective = strings.TrimSpace(objective)
	if objective != "" {
		session.todos = []todoItem{}
		session.runtime.replaceTodos(session.todos)
		if err := a.saveInteractiveSession(session); err != nil {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", err))
			return false
		}
	} else {
		current := cloneTodos(session.todos)
		if len(current) == 0 {
			a.writeInteractiveSystem("[goal] incomplete: checklist is empty; use /goal <objective> to start a goal")
			return false
		}
		if todosComplete(current) {
			a.writeInteractiveSystem("[goal] incomplete: checklist is already complete; use /goal <objective> to start a new goal")
			return false
		}
	}

	previous := cloneTodos(session.todos)
	unchanged := 0
	recoveryStreak := 0
	for iteration := 1; iteration <= a.cfg.maxGoalIterations; iteration++ {
		if ctx.Err() != nil {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: cancelled after %d iteration(s)", iteration-1))
			return true
		}
		a.writeInteractiveSystem(fmt.Sprintf("[goal] iteration %d/%d", iteration, a.cfg.maxGoalIterations))
		prompt := goalContinuationPrompt
		if objective != "" && iteration == 1 {
			prompt = fmt.Sprintf("Start working toward this objective: %s\n\nCreate a fresh authoritative checklist with update_todos before doing the work. Make concrete progress, verify each completed item, and do not claim success while any checklist item remains incomplete.", objective)
		}
		stopped, err := a.runInteractiveTurnResult(ctx, session, prompt)
		if err != nil {
			if persistErr := a.saveInteractiveSession(session); persistErr != nil {
				a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", persistErr))
				return false
			}
			if stopped || ctx.Err() != nil || errors.Is(err, context.Canceled) {
				a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: cancelled after %d iteration(s)", iteration))
				return true
			}
			if errors.Is(err, errSessionPersistence) {
				a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", err))
				return false
			}
			var toolFailure fatalToolFailure
			if errors.As(err, &toolFailure) {
				a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: fatal tool failure: %v", toolFailure))
				return false
			}
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: provider or tool failure: %v", err))
			return false
		}
		if stopped || ctx.Err() != nil {
			if persistErr := a.saveInteractiveSession(session); persistErr != nil {
				a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", persistErr))
				return false
			}
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: cancelled after %d iteration(s)", iteration))
			return true
		}
		if persistErr := a.saveInteractiveSession(session); persistErr != nil {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", persistErr))
			return false
		}
		current := cloneTodos(session.todos)
		if len(current) == 0 {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: checklist is empty after iteration %d", iteration))
			return false
		}
		if cancelled, ok := firstCancelledTodo(current); ok {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: checklist item %q was cancelled", cancelled.ID))
			return false
		}
		if todosComplete(current) {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] complete after %d iteration(s)", iteration))
			return false
		}
		if session.runtime.hadRecoverableToolError() {
			recoveryStreak++
		} else {
			recoveryStreak = 0
		}
		if recoveryStreak >= maxConsecutiveGoalRecoveries {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: recovery limit reached after %d iteration(s); %d consecutive recoverable tool-error turn(s)", iteration, recoveryStreak))
			return false
		}
		if equalTodos(previous, current) {
			unchanged++
		} else {
			unchanged = 0
		}
		if unchanged >= 2 {
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: stalled after %d iteration(s); checklist did not change", iteration))
			return false
		}
		previous = current
	}
	a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: iteration limit reached (%d)", a.cfg.maxGoalIterations))
	return false
}

const goalContinuationPrompt = "Continue working toward the objective. Make concrete progress on the next incomplete checklist item, then update the authoritative checklist with verified status. Do not claim success while any checklist item remains incomplete."

const maxConsecutiveGoalRecoveries = 3

var errSessionPersistence = errors.New("session persistence failure")

type sessionPersistenceError struct{ err error }

func newSessionPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	return sessionPersistenceError{err: err}
}

func (e sessionPersistenceError) Error() string { return e.err.Error() }
func (e sessionPersistenceError) Unwrap() error { return e.err }
func (e sessionPersistenceError) Is(target error) bool {
	return target == errSessionPersistence || errors.Is(e.err, target)
}

func syncRuntimeTodos(session *interactiveSession) {
	if session == nil || session.runtime == nil {
		return
	}
	session.runtime.todosMu.Lock()
	session.runtime.todos = cloneTodos(session.todos)
	session.runtime.todosMu.Unlock()
}

func todosComplete(todos []todoItem) bool {
	if len(todos) == 0 {
		return false
	}
	for _, todo := range todos {
		if todo.Status != todoStatusCompleted {
			return false
		}
	}
	return true
}

func firstCancelledTodo(todos []todoItem) (todoItem, bool) {
	for _, todo := range todos {
		if todo.Status == todoStatusCancelled {
			return todo, true
		}
	}
	return todoItem{}, false
}

func equalTodos(left, right []todoItem) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
