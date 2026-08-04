package app

import (
	"capelin-go/internal/contracts"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGoalContinuesAfterRecoverableApplicationToolErrors(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		arguments  string
		structured bool
	}{
		{name: "missing path", tool: toolReadFile, arguments: `{"path":"missing.txt"}`},
		{name: "invalid line range", tool: toolReadFile, arguments: `{"path":"present.txt","start_line":99}`},
		{name: "malformed arguments", tool: toolListFiles, arguments: `{`},
		{name: "disabled tool", tool: toolReadFile, arguments: `{"path":"present.txt"}`},
		{name: "non-zero command", tool: toolExecuteProgram, arguments: `{"command":"go","args":["tool","definitely-not-a-tool"]}`, structured: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testApp := newInteractiveTurnTestAppWithResponses(t,
				chatTurnResponse("", "", []map[string]any{
					goalToolCall("recover", test.tool, test.arguments),
					goalToolCall("seed", toolUpdateTodos, goalTodosArguments("work", "pending")),
				}),
				chatTurnResponse("recovered enough to continue", "", nil),
				chatTurnResponse("", "", []map[string]any{
					goalToolCall("complete", toolUpdateTodos, goalTodosArguments("work", "completed")),
				}),
				chatTurnResponse("done", "", nil),
			)
			if err := os.WriteFile(testApp.workspaceRoot+"/present.txt", []byte("one\ntwo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			allowed := map[string]bool{toolUpdateTodos: true}
			if test.name != "disabled tool" {
				allowed[test.tool] = true
			}
			configureGoalTestApp(testApp, allowed, 1, 3)
			var toolErrors []string
			testApp.app.sink.(*spySink).onToolResult = func(name string, isError bool, detail string) {
				if isError {
					toolErrors = append(toolErrors, name+":"+detail)
				}
			}
			session, err := testApp.app.newInteractiveSession(nil)
			if err != nil {
				t.Fatal(err)
			}

			if stopped := testApp.app.runGoal(context.Background(), session, "recover this task"); stopped {
				t.Fatal("recoverable goal unexpectedly stopped the REPL")
			}
			if !todosComplete(session.todos) {
				t.Fatalf("goal did not continue to completion: %#v", session.todos)
			}
			if len(testApp.userPrompts()) != 4 {
				t.Fatalf("provider calls=%d, want 4", len(testApp.userPrompts()))
			}
			if len(toolErrors) == 0 {
				t.Fatal("recoverable tool result was not emitted as an error event")
			}
			if test.structured && !strings.Contains(toolErrors[0], `"failed": true`) {
				t.Fatalf("structured command result was not preserved: %q", toolErrors[0])
			}

			snapshot, err := testApp.app.sessionStore.resolve(session.id)
			if err != nil {
				t.Fatal(err)
			}
			if !snapshotHasToolError(snapshot.Messages) {
				t.Fatal("saved goal conversation did not preserve the recoverable tool result")
			}
		})
	}
}

func TestGoalContinuationCanIssueAnotherToolAfterRecovery(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("missing", toolReadFile, `{"path":"missing.txt"}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("inspect", toolListFiles, `{"path":"."}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("complete", toolUpdateTodos, goalTodosArguments("work", "completed")),
		}),
		chatTurnResponse("finished", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolReadFile: true, toolListFiles: true, toolUpdateTodos: true}, 4, 1)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The continuation starts with an existing incomplete checklist so the
	// goal does not stop on an empty checklist before the model can recover.
	session.todos = []todoItem{{ID: "work", Content: "work", Status: todoStatusPending}}
	syncRuntimeTodos(session)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("continuation goal unexpectedly stopped the REPL")
	}
	if !todosComplete(session.todos) || len(testApp.userPrompts()) != 4 {
		t.Fatalf("continuation did not complete in one goal iteration: todos=%#v calls=%d", session.todos, len(testApp.userPrompts()))
	}
	if len(session.messages) < 7 || !snapshotHasToolError(session.messages) {
		t.Fatalf("conversation lost recovery continuation: %#v", session.messages)
	}
}

func TestGoalToolTimeoutRetriesOnceThenRecoversAsToolError(t *testing.T) {
	var requests atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-r.Context().Done()
	}))
	defer toolServer.Close()

	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("timeout", toolFetchPage, marshalTestJSON(map[string]any{"url": toolServer.URL, "timeout_seconds": 1})),
			goalToolCall("seed", toolUpdateTodos, goalTodosArguments("work", "pending")),
		}),
		chatTurnResponse("timeout was reported", "", nil),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("complete", toolUpdateTodos, goalTodosArguments("work", "completed")),
		}),
		chatTurnResponse("done", "", nil),
	)
	testApp.app.cfg.allowPrivateFetch = true
	configureGoalTestApp(testApp, map[string]bool{toolFetchPage: true, toolUpdateTodos: true}, 1, 2)
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.runGoal(context.Background(), session, "retry the slow tool"); stopped {
		t.Fatal("timeout recovery goal unexpectedly stopped the REPL")
	}
	if !todosComplete(session.todos) || requests.Load() != 2 {
		t.Fatalf("timeout retry/continuation mismatch: todos=%#v requests=%d", session.todos, requests.Load())
	}
	if !containsEvent(systemEvents, "timed out, retrying") {
		t.Fatalf("timeout retry event missing: %v", systemEvents)
	}
}

func TestGoalProviderFailureIsFatalAndIncomplete(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer server.Close()

	testApp := newInteractiveTurnTestApp(t)
	testApp.app.client.endpoint = server.URL
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.runGoal(context.Background(), session, "provider must work"); stopped {
		t.Fatal("provider failure unexpectedly stopped the REPL")
	}
	if !containsEvent(systemEvents, "provider or tool failure") || containsEvent(systemEvents, "recovery limit") {
		t.Fatalf("provider failure was not distinguishable: %v", systemEvents)
	}
	if requests.Load() != 1 {
		t.Fatalf("provider failure made %d calls, want 1", requests.Load())
	}
}

func TestGoalParentCancellationIsFatalCancellation(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if stopped := testApp.app.runGoal(ctx, session, "cancel this"); !stopped {
		t.Fatal("parent cancellation did not stop the REPL")
	}
	if !containsEvent(systemEvents, "cancelled") || len(testApp.userPrompts()) != 0 {
		t.Fatalf("cancellation outcome mismatch: events=%v calls=%d", systemEvents, len(testApp.userPrompts()))
	}
}

func TestGoalPersistenceFailureIsFatalIncomplete(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	testApp.app.sessionStore.writeAtomic = func(string, []byte) error { return errors.New("disk unavailable") }

	if stopped := testApp.app.runGoal(context.Background(), session, "persist this"); stopped {
		t.Fatal("persistence failure unexpectedly stopped the REPL")
	}
	if !containsEvent(systemEvents, "session persistence failure") || len(testApp.userPrompts()) != 0 {
		t.Fatalf("persistence failure outcome mismatch: events=%v calls=%d", systemEvents, len(testApp.userPrompts()))
	}
}

func TestGoalStopsAfterThreeConsecutiveRecoverableTurns(t *testing.T) {
	responses := make([]string, 0, maxConsecutiveGoalRecoveries*2)
	for i := 1; i <= maxConsecutiveGoalRecoveries; i++ {
		responses = append(responses,
			chatTurnResponse("", "", []map[string]any{
				goalToolCall("bad", "unknown_tool", `{}`),
				goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work "+string(rune('0'+i)), "pending")),
			}),
			chatTurnResponse("still working", "", nil),
		)
	}
	testApp := newInteractiveTurnTestAppWithResponses(t, responses...)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 10)
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "work", Content: "work", Status: todoStatusPending}}
	syncRuntimeTodos(session)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("recovery guard unexpectedly stopped the REPL")
	}
	if !containsEvent(systemEvents, "recovery limit reached") {
		t.Fatalf("bounded recovery outcome missing: %v", systemEvents)
	}
	if got := len(testApp.userPrompts()); got != maxConsecutiveGoalRecoveries*2 {
		t.Fatalf("provider calls=%d, want bounded %d", got, maxConsecutiveGoalRecoveries*2)
	}
}

func configureGoalTestApp(testApp *interactiveTurnTestApp, allowed map[string]bool, maxIterations, maxGoalIterations int) {
	testApp.app.cfg.yolo = true
	testApp.app.cfg.allowedTools = allowed
	testApp.app.cfg.maxIterations = maxIterations
	testApp.app.cfg.maxGoalIterations = maxGoalIterations
	testApp.app.cfg.toolRetryOnTimeout = true
	testApp.app.toolset = buildAgentTools(allowed)
}

func goalToolCall(id, name, arguments string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}
}

func goalTodosArguments(content, status string) string {
	raw, _ := json.Marshal(map[string]any{"todos": []map[string]string{{"id": "work", "content": content, "status": status}}})
	return string(raw)
}

func snapshotHasToolError(messages []contracts.Message) bool {
	for _, message := range messages {
		if message.Role == "tool" && (strings.Contains(message.Content, "Tool error:") || strings.Contains(message.Content, `"failed": true`)) {
			return true
		}
	}
	return false
}

func containsEvent(events []string, want string) bool {
	for _, event := range events {
		if strings.Contains(event, want) {
			return true
		}
	}
	return false
}
