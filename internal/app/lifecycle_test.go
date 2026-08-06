package app

import (
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionStoreResolvesValidSnapshotsAndSkipsMalformedNewest(t *testing.T) {
	store, err := newSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	first, err := store.create([]types.Message{{Role: "system", Content: "s"}})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second, err := store.create([]types.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "q"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.directory(), "00000000-0000-4000-8000-000000000000.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	newest, err := store.resolve("")
	if err != nil || newest.SessionUUID != second.SessionUUID {
		t.Fatalf("newest valid snapshot: got %#v, err=%v", newest, err)
	}
	prefix, err := store.resolve(first.SessionUUID[:8])
	if err != nil || prefix.SessionUUID != first.SessionUUID {
		t.Fatalf("prefix resolution: got %#v, err=%v", prefix, err)
	}
	if _, err := store.resolve("../"); err == nil {
		t.Fatal("path traversal selector unexpectedly accepted")
	}
	listed, err := store.list()
	if err != nil || len(listed) != 2 {
		t.Fatalf("valid session list: got %d, err=%v", len(listed), err)
	}
}

func TestSessionStoreFailedReplacementPreservesPreviousSnapshot(t *testing.T) {
	store, err := newSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.create([]types.Message{{Role: "system", Content: "before"}})
	if err != nil {
		t.Fatal(err)
	}
	store.writeAtomic = func(string, []byte) error { return errors.New("injected write failure") }
	snapshot.Messages = append(snapshot.Messages, types.Message{Role: "user", Content: "after"})
	if err := store.save(snapshot); err == nil {
		t.Fatal("injected save unexpectedly succeeded")
	}
	store.writeAtomic = func(path string, data []byte) error { return atomicWriteFile(path, data, 0o600) }
	loaded, err := store.resolve(snapshot.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "before" {
		t.Fatalf("failed replacement lost previous snapshot: %#v", loaded.Messages)
	}
}

func TestUpdateTodosRejectsInvalidReplacementAndPreservesPrevious(t *testing.T) {
	runtime := &agentRuntime{allowedTools: map[string]bool{toolUpdateTodos: true}}
	app := &app{cfg: config{allowedTools: map[string]bool{toolUpdateTodos: true}}}
	valid := `{"todos":[{"id":"one","content":"first","source":"test","status":"pending"}]}`
	if _, err := app.runToolForRuntime(context.Background(), runtime, types.ToolCall{Function: types.FunctionCall{Name: toolUpdateTodos, Arguments: valid}}); err != nil {
		t.Fatal(err)
	}
	invalid := `{"todos":[{"id":"one","content":"","status":"pending"}]}`
	if _, err := app.runToolForRuntime(context.Background(), runtime, types.ToolCall{Function: types.FunctionCall{Name: toolUpdateTodos, Arguments: invalid}}); err == nil {
		t.Fatal("invalid todo replacement unexpectedly succeeded")
	}
	todos := runtime.snapshotTodos()
	if len(todos) != 1 || todos[0].Content != "first" {
		t.Fatalf("invalid replacement changed previous state: %#v", todos)
	}
}

func TestGoalConfigIsIndependentAndRejectsInvalidLimits(t *testing.T) {
	t.Setenv("MAX_GOAL_ITERATIONS", "7")
	t.Setenv("MAX_ITERATIONS", "3")
	cfg, err := loadConfig([]string{"-i"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.maxGoalIterations != 7 || cfg.maxIterations != 3 {
		t.Fatalf("unexpected independent limits: goal=%d turn=%d", cfg.maxGoalIterations, cfg.maxIterations)
	}
	t.Setenv("MAX_GOAL_ITERATIONS", "0")
	if _, err := loadConfig([]string{"-i"}); err == nil || !strings.Contains(err.Error(), "MAX_GOAL_ITERATIONS") {
		t.Fatalf("invalid goal limit was not rejected: %v", err)
	}
}

func TestInteractiveGoalRequiresAndAcceptsCompletionHandshake(t *testing.T) {
	pendingArgs, _ := json.Marshal(map[string]any{"todos": []map[string]string{{"id": "one", "content": "first", "source": "model", "status": "pending"}}})
	completedArgs, _ := json.Marshal(map[string]any{"todos": []map[string]string{{"id": "one", "content": "first", "source": "model", "status": "completed"}}})
	toolResponse := func(calls ...map[string]any) string {
		return chatTurnResponse("", "", calls)
	}
	testApp := newInteractiveTurnTestAppWithResponses(t,
		toolResponse(goalToolCall("one", toolUpdateTodos, string(pendingArgs))),
		`{"choices":[{"message":{"role":"assistant","content":"working"}}]}`,
		toolResponse(goalToolCall("two", toolUpdateTodos, string(completedArgs))),
		`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`,
		toolResponse(goalToolCall("three", toolCompleteGoal, `{"summary":"implemented and verified","evidence":["the checklist is complete","the final state was verified"]}`)),
		`{"choices":[{"message":{"role":"assistant","content":"complete"}}]}`,
	)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.maxGoalIterations = 4
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runGoal(context.Background(), session, "finish the task"); stopped {
		t.Fatal("completed goal unexpectedly stopped the REPL")
	}
	if !todosComplete(session.todos) {
		t.Fatalf("goal completion did not follow authoritative todos: %#v", session.todos)
	}
	if got := testApp.userPrompts(); len(got) != 6 || !strings.Contains(got[0], "finish the task") || !strings.Contains(strings.Join(got, "\n"), "complete_goal") {
		t.Fatalf("goal did not append visible continuation turns: %#v", got)
	}
	if session.activeGoal == nil || !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("valid completion handshake was not retained: %#v", session.activeGoal)
	}
}

func TestInteractiveGoalDoesNotCompleteFromTodosAlone(t *testing.T) {
	completedArgs, _ := json.Marshal(map[string]any{"todos": []map[string]string{{"id": "one", "content": "first", "source": "model", "status": "completed"}}})
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("one", toolUpdateTodos, string(completedArgs))}),
		chatTurnResponse("done", "", nil),
		chatTurnResponse("still working", "", nil),
		chatTurnResponse("still working", "", nil),
	)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.maxGoalIterations = 2
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { events = append(events, message) }
	session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runGoal(context.Background(), session, "finish the task"); stopped {
		t.Fatal("goal unexpectedly stopped the REPL")
	}
	if !todosComplete(session.todos) || session.activeGoal == nil || session.activeGoal.Completion != nil {
		t.Fatalf("false completion changed goal state: todos=%#v goal=%#v", session.todos, session.activeGoal)
	}
	if containsEvent(events, "[goal] complete") || !containsEvent(events, "handshake is missing") {
		t.Fatalf("false completion outcome was not distinguishable: %v", events)
	}
}

func TestInteractiveSessionNewAndResumeKeepStateLocal(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	current, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "old"}})
	if err != nil {
		t.Fatal(err)
	}
	current.todos = []todoItem{{ID: "old", Content: "old work", Status: todoStatusInProgress}}
	syncRuntimeTodos(current)
	oldID := current.id
	if err := testApp.app.switchToNewSession(current); err != nil {
		t.Fatal(err)
	}
	if current.id == oldID || len(current.messages) != 1 || len(current.todos) != 0 {
		t.Fatalf("new session did not reset state: id=%s messages=%d todos=%#v", current.id, len(current.messages), current.todos)
	}
	newID := current.id
	if err := testApp.app.switchToSavedSession(current, oldID[:12]); err != nil {
		t.Fatal(err)
	}
	if current.id != oldID || len(current.messages) != 2 || len(current.todos) != 1 || current.todos[0].ID != "old" {
		t.Fatalf("resume did not restore old state: id=%s messages=%d todos=%#v", current.id, len(current.messages), current.todos)
	}
	if err := testApp.app.switchToSavedSession(current, newID); err != nil {
		t.Fatal(err)
	}
	if current.id != newID || len(current.todos) != 0 {
		t.Fatalf("resume changed session-local todo state unexpectedly: id=%s todos=%#v", current.id, current.todos)
	}
}

func TestInteractiveStartupResumeRestoresSnapshotWithoutProvider(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.create([]types.Message{
		{Role: "system", Content: "test"},
		{Role: "user", Content: "remember this"},
		{Role: "assistant", Content: "I remember"},
	})
	if err != nil {
		t.Fatal(err)
	}
	testApp.app.cfg.resumeRequested = true
	testApp.app.cfg.resumeID = snapshot.SessionUUID
	resumed, err := testApp.app.startInteractiveSession()
	if err != nil {
		t.Fatal(err)
	}
	if resumed.id != snapshot.SessionUUID || len(resumed.messages) != 3 || resumed.messages[1].Content != "remember this" {
		t.Fatalf("startup resume did not restore snapshot: id=%s messages=%#v", resumed.id, resumed.messages)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("startup resume made a provider request: %#v", got)
	}
}

func TestInteractiveResumeFailureLeavesCurrentSessionUnchanged(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	current, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "current"}})
	if err != nil {
		t.Fatal(err)
	}
	originalID := current.id
	originalMessages := cloneMessages(current.messages)
	if err := testApp.app.switchToSavedSession(current, "not-a-session-path"); err == nil {
		t.Fatal("invalid resume selector unexpectedly succeeded")
	}
	if current.id != originalID || len(current.messages) != len(originalMessages) || current.messages[1].Content != originalMessages[1].Content {
		t.Fatalf("failed resume replaced current session: id=%s messages=%#v", current.id, current.messages)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("failed resume made a provider request: %#v", got)
	}
}

func TestInteractiveReadlineExitPersistsInitialSnapshot(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, "/exit\r", "")
	if err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	); err != nil {
		t.Fatal(err)
	}
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || len(snapshots[0].Messages) != 1 {
		t.Fatalf("exit did not persist exactly one initial session: %#v", snapshots)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("exit unexpectedly reached model: %#v", got)
	}
}
func TestSessionStoreDerivesLegacyMetadataAndRoundTripsNames(t *testing.T) {
	store, err := newSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := sessionSnapshot{
		SessionUUID: generateUUID(),
		Messages: []types.Message{
			{Role: "system", Content: "test"},
			{Role: "user", Content: "  first   meaningful prompt  "},
			{Role: "assistant", Content: "answer"},
			{Role: "user", Content: "latest prompt"},
		},
		Name:  "  named session  ",
		Todos: []todoItem{},
	}
	if err := store.save(snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.resolve(snapshot.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "named session" || loaded.Topic != "first   meaningful prompt" || loaded.LastInput != "latest prompt" {
		t.Fatalf("session metadata did not round-trip: %#v", loaded)
	}
	legacy := snapshot
	legacy.Name = ""
	legacy.Topic = ""
	legacy.LastInput = ""
	if err := store.save(legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.resolve(snapshot.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Topic != "first   meaningful prompt" || loaded.LastInput != "latest prompt" {
		t.Fatalf("legacy metadata was not derived: topic=%q input=%q", loaded.Topic, loaded.LastInput)
	}
}

func TestHistoricalNamedSessionRemainsLoadableResumableAndListed(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	store := testApp.app.sessionStore
	if store == nil {
		var err error
		store, err = newSessionStore(testApp.workspaceRoot)
		if err != nil {
			t.Fatal(err)
		}
		testApp.app.sessionStore = store
	}
	historical := sessionSnapshot{
		SessionUUID: generateUUID(),
		Messages: []types.Message{
			{Role: "system", Content: "test"},
			{Role: "user", Content: "historical prompt"},
			{Role: "assistant", Content: "historical answer"},
		},
		Name:      "  historical name  ",
		Topic:     "derived topic",
		LastInput: "historical prompt",
		Todos:     []todoItem{},
	}
	if err := store.save(historical); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.resolve(historical.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "historical name" {
		t.Fatalf("historical name did not load: %#v", loaded)
	}
	if got := sessionDisplayLabel(loaded); got != "historical name" {
		t.Fatalf("name-first display label = %q, want historical name", got)
	}

	current, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "current"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.app.switchToSavedSession(current, historical.SessionUUID); err != nil {
		t.Fatalf("resume historical session: %v", err)
	}
	if current.name != "historical name" || len(current.messages) != len(historical.Messages) {
		t.Fatalf("resumed historical session lost metadata or messages: name=%q messages=%d", current.name, len(current.messages))
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = write
	listErr := testApp.app.listInteractiveSessions(current)
	_ = write.Close()
	os.Stderr = originalStderr
	listing, readErr := io.ReadAll(read)
	_ = read.Close()
	if listErr != nil {
		t.Fatalf("list historical session: %v", listErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(listing), historical.SessionUUID) || !strings.Contains(string(listing), "historical name") {
		t.Fatalf("historical name was not present in session list: %q", listing)
	}
}

func TestInteractiveSessionListMarksCurrentAndShowsProgress(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "list this"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "one", Content: "work", Status: todoStatusCompleted}, {ID: "two", Content: "more", Status: todoStatusPending}}
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	if err := testApp.app.listInteractiveSessions(session); err != nil {
		t.Fatal(err)
	}
	if got := displayTodoProgress(session.todos); got != " | todos: 1/2 completed" {
		t.Fatalf("unexpected todo progress: %q", got)
	}
}

func TestSessionSnapshotWithoutTodosLoadsWithEmptyChecklist(t *testing.T) {
	store, err := newSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.create([]types.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	path := store.pathFor(snapshot.SessionUUID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "todos")
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.resolve(snapshot.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Messages[1].Content != "legacy" || len(loaded.Todos) != 0 {
		t.Fatalf("legacy snapshot did not restore conversation with empty todos: %#v", loaded)
	}
}

func TestUpdateTodosRejectsDuplicateAndUnsupportedItems(t *testing.T) {
	runtime := &agentRuntime{allowedTools: map[string]bool{toolUpdateTodos: true}}
	app := &app{cfg: config{allowedTools: map[string]bool{toolUpdateTodos: true}}}
	valid := `{"todos":[{"id":"one","content":"first","status":"pending"}]}`
	if _, err := app.runToolForRuntime(context.Background(), runtime, types.ToolCall{Function: types.FunctionCall{Name: toolUpdateTodos, Arguments: valid}}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`{"todos":[{"id":"one","content":"first","status":"pending"},{"id":"one","content":"again","status":"completed"}]}`,
		`{"todos":[{"id":"two","content":"first","status":"unknown"}]}`,
	} {
		if _, err := app.runToolForRuntime(context.Background(), runtime, types.ToolCall{Function: types.FunctionCall{Name: toolUpdateTodos, Arguments: invalid}}); err == nil {
			t.Fatalf("invalid todo replacement unexpectedly succeeded: %s", invalid)
		}
	}
	if todos := runtime.snapshotTodos(); len(todos) != 1 || todos[0].ID != "one" {
		t.Fatalf("invalid todo updates changed valid state: %#v", todos)
	}
}
func TestGoalRejectsNonYoloBeforeProviderOrStateChange(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runGoal(context.Background(), session, "objective"); stopped {
		t.Fatal("non-YOLO goal unexpectedly stopped the REPL")
	}
	if len(session.todos) != 0 || len(session.messages) != 1 || len(testApp.userPrompts()) != 0 {
		t.Fatalf("non-YOLO goal changed state or called provider: todos=%#v messages=%#v requests=%#v", session.todos, session.messages, testApp.userPrompts())
	}
}

func TestBareGoalRejectsEmptyButResumesCompletedChecklistForHandshake(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		testApp := newInteractiveTurnTestApp(t)
		testApp.app.cfg.yolo = true
		testApp.app.cfg.maxGoalIterations = 2
		session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
			t.Fatal("bare goal unexpectedly stopped the REPL")
		}
		if len(testApp.userPrompts()) != 0 {
			t.Fatalf("empty recovery goal called provider: %#v", testApp.userPrompts())
		}
	})
	t.Run("completed", func(t *testing.T) {
		testApp := newInteractiveTurnTestAppWithResponses(t,
			chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"resumed","evidence":["final checklist was already complete"]}`)}),
			chatTurnResponse("resumed", "", nil),
		)
		testApp.app.cfg.yolo = true
		testApp.app.cfg.maxGoalIterations = 2
		session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		session.todos = []todoItem{{ID: "done", Content: "already done", Status: todoStatusCompleted}}
		syncRuntimeTodos(session)
		if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
			t.Fatal("bare goal unexpectedly stopped the REPL")
		}
		if len(testApp.userPrompts()) != 2 || session.activeGoal == nil || !validGoalCompletion(session.activeGoal, session.todos) {
			t.Fatalf("completed recovery did not establish handshake: calls=%#v goal=%#v", testApp.userPrompts(), session.activeGoal)
		}
	})
}

func TestGoalStopsAfterTwoUnchangedIncompleteSnapshots(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"todos": []map[string]string{{"id": "one", "content": "first", "source": "model", "status": "pending"}}})
	toolResponse := func(id string) string {
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"update_todos","arguments":` + string(mustJSONQuote(args)) + `}}]}}]}`
	}
	testApp := newInteractiveTurnTestAppWithResponses(t, toolResponse("one"), `{"choices":[{"message":{"role":"assistant","content":"progress"}}]}`, toolResponse("two"), `{"choices":[{"message":{"role":"assistant","content":"progress"}}]}`, toolResponse("three"), `{"choices":[{"message":{"role":"assistant","content":"progress"}}]}`)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.maxGoalIterations = 5
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runGoal(context.Background(), session, "objective"); stopped {
		t.Fatal("stalled goal unexpectedly stopped the REPL")
	}
	if got := len(testApp.userPrompts()); got != 6 {
		t.Fatalf("stalled goal made %d turns, want 5", got)
	}
}
func mustJSONQuote(raw []byte) []byte {
	encoded, _ := json.Marshal(string(raw))
	return encoded
}
