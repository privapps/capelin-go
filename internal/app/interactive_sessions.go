package app

import (
	"capelin-go/internal/contracts"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// leadingCommandArgument recognizes a command only when it occupies the
// beginning of the trimmed request. Keeping the boundary check here prevents
// slash-like text such as /goalist or embedded prose from becoming commands.
// It is the shared application seam for one-shot requests, interactive initial
// prompts, and the interactive /goal command.
func leadingCommandArgument(input, command string) (string, bool) {
	input = strings.TrimSpace(input)
	if input == command {
		return "", true
	}
	if !strings.HasPrefix(input, command) {
		return "", false
	}
	remainder := strings.TrimPrefix(input, command)
	separator, _ := utf8.DecodeRuneInString(remainder)
	if unicode.IsSpace(separator) {
		return strings.TrimSpace(remainder), true
	}
	return "", false
}

func interactiveCommandArgument(input, command string) (string, bool) {
	return leadingCommandArgument(input, command)
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
		activeGoal:    cloneGoalState(snapshot.ActiveGoal),
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
	updateSessionTodos := func(todos []todoItem) {
		session.todos = cloneTodos(todos)
		if session.activeGoal != nil {
			session.activeGoal.Completion = nil
		}
	}
	if session.ephemeral {
		session.runtime.todosChanged = updateSessionTodos
		return
	}
	session.runtime.todosChanged = func(todos []todoItem) {
		updateSessionTodos(todos)
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
	if session == nil || session.ephemeral || strings.TrimSpace(session.id) == "" {
		return nil
	}
	return a.persistInteractiveSession(session)
}

// saveInteractiveSessionCandidate persists a worker session before it is
// committed to the live session. The worker is intentionally ephemeral so
// that a provider or persistence failure cannot mutate the live session.
func (a *app) saveInteractiveSessionCandidate(session *interactiveSession) error {
	if session == nil || strings.TrimSpace(session.id) == "" {
		return nil
	}
	return a.persistInteractiveSession(session)
}

func (a *app) persistInteractiveSession(session *interactiveSession) error {
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
		ActiveGoal:    cloneGoalForPersistence(session.activeGoal),
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
	goalStartedAt := time.Now()
	if session == nil {
		a.writeInteractiveSystem("[goal] incomplete: interactive session is nil")
		return false
	}
	if !a.cfg.yolo {
		a.writeInteractiveSystem("[goal] --yolo is required before using /goal")
		return false
	}
	session.goalCompleted = false
	goalProfile := a.cfg.goalRuntimeProfile()
	if goalProfile.MaxGoalIterations <= 0 {
		a.writeInteractiveSystem("[goal] invalid goal iteration limit")
		return false
	}
	if session.runtime == nil {
		session.runtime = a.rootRuntime()
		a.attachInteractiveRuntime(session)
	}
	restoreProfile := session.runtime.selectExecutionProfile(goalProfile)
	defer restoreProfile()
	objective = strings.TrimSpace(objective)
	originalTodos := cloneTodos(session.todos)
	originalGoal := cloneGoalState(session.activeGoal)
	if objective != "" {
		session.activeGoal = &goalState{Objective: objective, Generation: nextGoalGeneration(session.activeGoal)}
		session.todos = []todoItem{}
		session.runtime.replaceTodos(session.todos)
		if err := a.saveGoalSession(session); err != nil {
			session.todos = originalTodos
			session.activeGoal = originalGoal
			syncRuntimeTodos(session)
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", err))
			return false
		}
	} else {
		current := cloneTodos(session.todos)
		if len(current) == 0 && session.activeGoal == nil {
			a.writeInteractiveSystem("[goal] incomplete: checklist is empty; use /goal <objective> to start a goal")
			return false
		}
		if session.activeGoal == nil {
			session.activeGoal = &goalState{Objective: "Resume the current checklist", Generation: 1}
		}
		if session.activeGoal.Generation == 0 {
			session.activeGoal.Generation = 1
		}
		if strings.TrimSpace(session.activeGoal.Objective) == "" {
			session.activeGoal.Objective = "Resume the current checklist"
		}
		if validGoalCompletion(session.activeGoal, current) {
			session.goalCompleted = true
			a.writeInteractiveSystem("[goal] complete: the persisted completion handshake is valid")
			return false
		}
		// A resumed run must re-establish a claim against its current final
		// checklist. A stale or provisional claim is never carried forward.
		session.activeGoal.Completion = nil
		if err := a.saveGoalSession(session); err != nil {
			session.activeGoal = originalGoal
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", err))
			return false
		}
	}
	// Distinguish a goal worker from an ordinary turn in a session that may
	// retain completed goal metadata. This private marker is only set on an
	// ephemeral worker and is cleared when the worker is committed.
	session.goalTurn = session.ephemeral
	if session.runtime.allowedTools == nil {
		session.runtime.allowedTools = make(map[string]bool)
	}
	session.runtime.allowedTools[toolCompleteGoal] = true
	heartbeat := newGoalHeartbeatAt(goalStartedAt, a.writeInteractiveSystem, goalProfile.MaxGoalIterations, a.goalHeartbeatInitialDelay, a.goalHeartbeatCadence, goalHeartbeatProgress{
		todosSnapshot: session.runtime.snapshotTodos,
		agentSnapshot: a.subagents.ListAll,
	})
	defer heartbeat.stop()
	terminalGoalStatus := func(message string) {
		heartbeat.stop()
		a.writeInteractiveSystem(message)
	}
	session.runtime.enableGoal(session.activeGoal.Generation)
	defer func() {
		delete(session.runtime.allowedTools, toolCompleteGoal)
		session.runtime.disableGoal()
	}()

	previous := cloneTodos(session.todos)
	unchanged := 0
	recoveryStreak := 0
	for iteration := 1; iteration <= goalProfile.MaxGoalIterations; iteration++ {
		if ctx.Err() != nil {
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: cancelled after %d iteration(s)", iteration-1))
			_ = a.saveGoalSession(session)
			return true
		}
		heartbeat.beginIteration(iteration)
		prompt := goalContinuationPrompt
		if objective != "" && iteration == 1 {
			prompt = fmt.Sprintf("Start working toward this objective: %s\n\nCreate a fresh authoritative checklist with update_todos before doing the work. Make concrete progress, verify each completed item, and do not claim success while any checklist item remains incomplete. When the final checklist is complete, call complete_goal with a concise summary and non-empty evidence statements. %s", objective, goalParallelismGuidance)
		} else if todosComplete(session.todos) {
			prompt = fmt.Sprintf("Continue working toward the objective %q. The authoritative checklist is complete, but the completion handshake is missing or stale. Verify the final state and call complete_goal with a non-empty summary and evidence list; do not change the checklist unless verification requires it. %s", session.activeGoal.Objective, goalParallelismGuidance)
		} else if session.activeGoal != nil {
			prompt = fmt.Sprintf("Continue working toward the objective %q. Make concrete progress on the next incomplete checklist item, then update the authoritative checklist with verified status. When every item is complete, call complete_goal with a non-empty summary and evidence list. %s", session.activeGoal.Objective, goalParallelismGuidance)
		}
		stopped, err := a.runInteractiveTurnResult(ctx, session, prompt)
		heartbeat.endTurn()
		if err != nil {
			if persistErr := a.saveGoalSession(session); persistErr != nil {
				terminalGoalStatus(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", persistErr))
				return false
			}
			if stopped || ctx.Err() != nil || errors.Is(err, context.Canceled) {
				terminalGoalStatus(fmt.Sprintf("[goal] incomplete: cancelled after %d iteration(s)", iteration))
				return true
			}
			if errors.Is(err, errSessionPersistence) {
				terminalGoalStatus(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", err))
				return false
			}
			var toolFailure fatalToolFailure
			if errors.As(err, &toolFailure) {
				terminalGoalStatus(fmt.Sprintf("[goal] incomplete: fatal tool failure: %v", toolFailure))
				return false
			}
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: provider or tool failure: %v", err))
			return false
		}
		if stopped || ctx.Err() != nil {
			if persistErr := a.saveGoalSession(session); persistErr != nil {
				terminalGoalStatus(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", persistErr))
				return false
			}
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: cancelled after %d iteration(s)", iteration))
			return true
		}
		if persistErr := a.saveGoalSession(session); persistErr != nil {
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", persistErr))
			return false
		}
		current := cloneTodos(session.todos)
		if len(current) == 0 {
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: checklist is empty after iteration %d", iteration))
			return false
		}
		if cancelled, ok := firstCancelledTodo(current); ok {
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: checklist item %q was cancelled", cancelled.ID))
			return false
		}
		if todosComplete(current) {
			if validGoalCompletion(session.activeGoal, current) {
				if persistErr := a.saveGoalSession(session); persistErr != nil {
					terminalGoalStatus(fmt.Sprintf("[goal] incomplete: session persistence failure: %v", persistErr))
					return false
				}
				session.goalCompleted = true
				terminalGoalStatus(fmt.Sprintf("[goal] complete after %d iteration(s): %s", iteration, session.activeGoal.Completion.Summary))
				return false
			}
			a.writeInteractiveSystem(fmt.Sprintf("[goal] incomplete: checklist is complete after iteration %d, but the complete_goal handshake is missing or stale; continuing", iteration))
		}
		if session.runtime.hadRecoverableToolError() {
			recoveryStreak++
		} else {
			recoveryStreak = 0
		}
		if recoveryStreak >= maxConsecutiveGoalRecoveries {
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: recovery limit reached after %d iteration(s); %d consecutive recoverable tool-error turn(s)", iteration, recoveryStreak))
			return false
		}
		if equalTodos(previous, current) {
			unchanged++
		} else {
			unchanged = 0
		}
		if unchanged >= 2 {
			terminalGoalStatus(fmt.Sprintf("[goal] incomplete: stalled after %d iteration(s); checklist did not change", iteration))
			return false
		}
		previous = current
	}
	terminalGoalStatus(fmt.Sprintf("[goal] incomplete: iteration limit reached (%d)", goalProfile.MaxGoalIterations))
	return false
}

func (a *app) saveGoalSession(session *interactiveSession) error {
	if session != nil && session.ephemeral {
		return a.saveInteractiveSessionCandidate(session)
	}
	return a.saveInteractiveSession(session)
}

const goalParallelismGuidance = "When subagent tools are available and the work has independent pieces, create subagents with create_subagent, start them in parallel with run_subagent using wait=false, then await their results and integrate and verify them."

const goalContinuationPrompt = "Continue working toward the objective. Make concrete progress on the next incomplete checklist item, then update the authoritative checklist with verified status. When every item is complete, call complete_goal with a non-empty summary and evidence list. Do not claim success while any checklist item remains incomplete. " + goalParallelismGuidance

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
