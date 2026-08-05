package app

import (
	"bytes"
	"capelin-go/internal/contracts"
	"capelin-go/internal/skills"
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startInteractiveSessionPipe(t *testing.T, testApp *interactiveTurnTestApp, session *interactiveSession, historyFile string) (*interactiveInputPipe, <-chan error) {
	t.Helper()
	pipe := newInteractiveInputPipe()
	rl := newInteractiveTestReadlineFromReader(t, pipe, historyFile)
	done := make(chan error, 1)
	go func() {
		done <- testApp.app.runInteractiveReadlineSession(context.Background(), session, rl)
	}()
	return pipe, done
}

func TestInteractiveCompactionPreservesSessionStateAndReloadsSkill(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{"choices":[{"message":{"role":"assistant","content":"goals, decisions, and unfinished work"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"follow-up response"}}]}`,
	)
	testApp.app.skills = map[string]skills.Skill{
		"research": {Name: "research", Content: "full research guidance"},
	}
	session, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "test system prompt"},
		{Role: "user", Content: "original goal"},
		{Role: "assistant", Content: "historical answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	session.name = "named session"
	session.topic = "durable topic"
	session.lastInput = "last direct input"
	session.lastResponse = "latest ordinary response"
	session.loadedSkills = map[string]bool{"research": true}
	session.providerState = &contracts.ContinuationState{
		Provider: "test-provider",
		Version:  1,
		Data:     json.RawMessage(`{"continuation":"opaque"}`),
	}
	session.todos = []todoItem{{ID: "one", Content: "unfinished work", Status: todoStatusInProgress}}
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	originalID := session.id
	originalCreatedAt := session.createdAt
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }

	pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
	pipe.send("/compact\r")
	testApp.waitForTurn(t)

	if session.id != originalID || !session.createdAt.Equal(originalCreatedAt) {
		t.Fatalf("compaction changed session identity: id=%q created=%s", session.id, session.createdAt)
	}
	if session.name != "named session" || session.topic != "durable topic" || session.lastInput != "last direct input" || session.lastResponse != "latest ordinary response" {
		t.Fatalf("compaction changed session metadata: %#v", session)
	}
	if !reflect.DeepEqual(session.todos, []todoItem{{ID: "one", Content: "unfinished work", Status: todoStatusInProgress}}) {
		t.Fatalf("compaction changed checklist: %#v", session.todos)
	}
	if session.providerState != nil {
		t.Fatalf("provider continuation state survived compaction: %#v", session.providerState)
	}
	if len(session.loadedSkills) != 0 {
		t.Fatalf("loaded skill bookkeeping survived compaction: %#v", session.loadedSkills)
	}
	if len(session.messages) != 2 || session.messages[0].Content != "test system prompt" || session.messages[1].Role != "assistant" {
		t.Fatalf("unexpected compacted session: %#v", session.messages)
	}
	if !strings.HasPrefix(session.messages[1].Content, compactedHistoryMarker) || strings.Contains(session.messages[1].Content, "Summarize the conversation below") {
		t.Fatalf("compacted summary was not safely marked: %#v", session.messages[1])
	}
	if len(testApp.requests) != 1 || len(testApp.requests[0].Tools) != 0 {
		t.Fatalf("compaction request exposed tools or made extra requests: %#v", testApp.requests)
	}
	if !containsEvent(systemEvents, "compacted conversation to 2 messages") {
		t.Fatalf("successful compaction did not report success after persistence: %v", systemEvents)
	}

	pipe.send("$research follow-up\r")
	testApp.waitForTurn(t)
	if len(testApp.requests) != 2 {
		t.Fatalf("request count after skill reload=%d want 2", len(testApp.requests))
	}
	followUp := testApp.requests[1].Messages[len(testApp.requests[1].Messages)-1].Content
	if !strings.Contains(followUp, "full research guidance") || strings.Contains(followUp, "research already loaded") {
		t.Fatalf("explicit skill reference was not reloadable after compaction: %q", followUp)
	}
	if !session.loadedSkills["research"] {
		t.Fatal("skill reload was not committed after the successful follow-up")
	}

	_ = pipe.Close()
	if err := <-done; err != nil {
		t.Fatalf("interactive compaction/resume loop: %v", err)
	}
	snapshot, err := testApp.app.sessionStore.resolve(originalID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionUUID != originalID || snapshot.Name != "named session" || snapshot.Topic != "durable topic" || snapshot.LastInput != "$research follow-up" {
		t.Fatalf("persisted metadata was not preserved: %#v", snapshot)
	}
	if len(snapshot.Todos) != 1 || snapshot.Todos[0].Status != todoStatusInProgress {
		t.Fatalf("persisted checklist was not preserved: %#v", snapshot.Todos)
	}

	testApp.app.cfg.resumeRequested = true
	testApp.app.cfg.resumeID = originalID
	resumed, err := testApp.app.startInteractiveSession()
	if err != nil {
		t.Fatalf("resume compacted session: %v", err)
	}
	if resumed.id != originalID || len(resumed.messages) < 2 || !strings.HasPrefix(resumed.messages[1].Content, compactedHistoryMarker) {
		t.Fatalf("resumed session did not contain compacted history: %#v", resumed)
	}
	if resumed.name != "named session" || resumed.topic != "durable topic" || len(resumed.todos) != 1 {
		t.Fatalf("resumed metadata/checklist mismatch: %#v", resumed)
	}
}

func TestInteractiveCompactionPreservesLatestResponseForSave(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{"choices":[{"message":{"role":"assistant","content":"ordinary response"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"compact summary"}}]}`,
	)
	pipe, done := startInteractiveReadlinePipe(t, testApp, "")
	pipe.send("ordinary prompt\r")
	testApp.waitForTurn(t)
	pipe.send("/compact\r")
	testApp.waitForTurn(t)
	pipe.send("/save\r")
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(testApp.workspaceRoot, interactiveResponseFile))
		if err == nil {
			if string(data) != "ordinary response" {
				t.Fatalf("/save wrote compaction output instead of ordinary response: %q", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/save did not create response file: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = pipe.Close()
	if err := <-done; err != nil {
		t.Fatalf("interactive save after compaction: %v", err)
	}
}

func TestInteractiveCompactionIsDocumentedInHelpAndSynchronousUsageErrors(t *testing.T) {
	var help bytes.Buffer
	PrintUsage(&help, "capelin-go")
	if !strings.Contains(help.String(), "/compact") || !strings.Contains(help.String(), "accepts no arguments") {
		t.Fatalf("interactive compaction help is missing: %q", help.String())
	}

	for _, input := range []string{"/compact now", "/compact\tnow", "/compact\nnow", "/compact\u00a0now"} {
		t.Run(fmt.Sprintf("separator-%q", input), func(t *testing.T) {
			testApp := newInteractiveTurnTestApp(t)
			session := &interactiveSession{messages: []types.Message{{Role: "system", Content: "system"}}}
			if testApp.app.handleInteractiveInput(context.Background(), session, input) {
				t.Fatal("synchronous compaction usage error unexpectedly stopped the session")
			}
			if len(testApp.requests) != 0 {
				t.Fatalf("synchronous compaction usage error reached the model: %#v", testApp.requests)
			}
		})
	}
}

func TestInteractiveCompactionFallbackDispatcherCompactsWithoutReadline(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, `{"choices":[{"message":{"role":"assistant","content":"fallback summary"}}]}`)
	session := &interactiveSession{messages: []types.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "history"}}}
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "/compact\tunexpected") {
		t.Fatal("fallback compaction usage error unexpectedly stopped the session")
	}
	if len(testApp.requests) != 0 {
		t.Fatalf("fallback compaction usage error reached the model: %#v", testApp.requests)
	}
	if testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "/compact") {
		t.Fatal("fallback compaction unexpectedly stopped the session")
	}
	controller.wait()
	if len(testApp.requests) != 1 || len(testApp.requests[0].Tools) != 0 || !containsMessage(session.messages, compactedHistoryMarker) {
		t.Fatalf("fallback compaction did not commit a no-tools summary: requests=%#v messages=%#v", testApp.requests, session.messages)
	}
}

func TestInteractiveCompactionPersistenceFailureRollsBackAndRecovers(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{"choices":[{"message":{"role":"assistant","content":"should not commit"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"recovered response"}}]}`,
	)
	session, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "test system"},
		{Role: "user", Content: "original history"},
	})
	if err != nil {
		t.Fatal(err)
	}
	session.name = "before failure"
	session.topic = "before topic"
	session.lastInput = "before input"
	session.lastResponse = "before response"
	session.loadedSkills = map[string]bool{"research": true}
	session.providerState = &contracts.ContinuationState{Provider: "test", Version: 1, Data: json.RawMessage(`{"state":true}`)}
	session.todos = []todoItem{{ID: "one", Content: "keep this", Status: todoStatusPending}}
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	beforeMessages := cloneMessages(session.messages)
	beforeState := cloneProviderState(session.providerState)
	beforeSkills := map[string]bool{"research": true}
	beforeTodos := cloneTodos(session.todos)
	beforeSnapshot, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }
	testApp.app.sessionStore.writeAtomic = func(string, []byte) error { return errors.New("disk unavailable") }

	pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
	pipe.send("/compact\r")
	testApp.waitForTurn(t)
	if !reflect.DeepEqual(session.messages, beforeMessages) || !reflect.DeepEqual(session.providerState, beforeState) || !reflect.DeepEqual(session.loadedSkills, beforeSkills) || !reflect.DeepEqual(session.todos, beforeTodos) || session.name != "before failure" || session.lastResponse != "before response" {
		t.Fatalf("persistence failure changed live session: %#v", session)
	}
	afterFailure, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterFailure, beforeSnapshot) {
		t.Fatalf("persistence failure replaced previous snapshot:\nbefore=%#v\nafter=%#v", beforeSnapshot, afterFailure)
	}
	if !containsEvent(systemEvents, "could not save session") || containsEvent(systemEvents, "compacted conversation") {
		t.Fatalf("persistence failure output was misleading: %v", systemEvents)
	}

	testApp.app.sessionStore.writeAtomic = func(path string, data []byte) error { return atomicWriteFile(path, data, 0o600) }
	pipe.send("recover prompt\r")
	testApp.waitForTurn(t)
	_ = pipe.Close()
	if err := <-done; err != nil {
		t.Fatalf("recovery after failed compaction: %v", err)
	}
	if len(testApp.requests) != 2 {
		t.Fatalf("request count after recovery=%d want 2", len(testApp.requests))
	}
	recoveryMessages := testApp.requests[1].Messages
	if !containsMessage(recoveryMessages, "original history") || containsMessage(recoveryMessages, compactedHistoryMarker) {
		t.Fatalf("recovery did not use the original conversation: %#v", recoveryMessages)
	}
}

func TestInteractiveCompactionRejectsProviderFailuresWithoutMutation(t *testing.T) {
	toolResponse := `{"choices":[{"message":{"role":"assistant","content":"bad","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`
	for _, test := range []struct {
		name     string
		response string
		cancel   bool
	}{
		{name: "provider failure", response: `{`},
		{name: "empty summary", response: `{"choices":[{"message":{"role":"assistant","content":"   "}}]}`},
		{name: "unsupported tool call", response: toolResponse},
		{name: "cancelled", response: `{"choices":[{"message":{"role":"assistant","content":"unused"}}]}`, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testApp := newInteractiveTurnTestAppWithResponses(t, test.response)
			session := &interactiveSession{
				messages:      []types.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "history"}},
				providerState: &contracts.ContinuationState{Provider: "test", Version: 1},
				loadedSkills:  map[string]bool{"research": true},
				todos:         []todoItem{{ID: "one", Content: "work", Status: todoStatusPending}},
			}
			before := *session
			before.messages = cloneMessages(session.messages)
			before.providerState = cloneProviderState(session.providerState)
			before.loadedSkills = map[string]bool{"research": true}
			before.todos = cloneTodos(session.todos)
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := testApp.app.compactInteractiveSession(ctx, session); err == nil {
				t.Fatal("invalid compaction unexpectedly succeeded")
			}
			if !reflect.DeepEqual(session.messages, before.messages) || !reflect.DeepEqual(session.providerState, before.providerState) || !reflect.DeepEqual(session.loadedSkills, before.loadedSkills) || !reflect.DeepEqual(session.todos, before.todos) {
				t.Fatalf("failed compaction changed session: %#v", session)
			}
		})
	}
}

func TestInteractiveCompactionControllerDiscardsProviderErrorAndRecovers(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{`,
		`{"choices":[{"message":{"role":"assistant","content":"recovered"}}]}`,
	)
	session, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "original history"},
	})
	if err != nil {
		t.Fatal(err)
	}
	session.lastResponse = "prior response"
	session.loadedSkills = map[string]bool{"research": true}
	session.providerState = &contracts.ContinuationState{Provider: "test", Version: 1}
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	beforeMessages := cloneMessages(session.messages)
	beforeState := cloneProviderState(session.providerState)
	beforeSkills := map[string]bool{"research": true}
	var systemEvents []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systemEvents = append(systemEvents, message) }

	pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
	pipe.send("/compact\r")
	testApp.waitForTurn(t)
	if !reflect.DeepEqual(session.messages, beforeMessages) || !reflect.DeepEqual(session.providerState, beforeState) || !reflect.DeepEqual(session.loadedSkills, beforeSkills) || session.lastResponse != "prior response" {
		t.Fatalf("controller committed provider-error worker: %#v", session)
	}
	if !containsEvent(systemEvents, "error:") || containsEvent(systemEvents, "compacted conversation") {
		t.Fatalf("provider failure output was misleading: %v", systemEvents)
	}

	pipe.send("recover prompt\r")
	testApp.waitForTurn(t)
	_ = pipe.Close()
	if err := <-done; err != nil {
		t.Fatalf("recovery after provider failure: %v", err)
	}
	if len(testApp.requests) != 2 || !containsMessage(testApp.requests[1].Messages, "original history") || containsMessage(testApp.requests[1].Messages, compactedHistoryMarker) {
		t.Fatalf("provider failure did not recover original conversation: %#v", testApp.requests)
	}
}

func TestInteractiveCompactionBusyInputRejectsAndCancellationRecovers(t *testing.T) {
	workspace := t.TempDir()
	var requests atomic.Int32
	var recoveredPrompt atomic.Value
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request types.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if requests.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		for _, message := range request.Messages {
			if message.Role == "user" {
				recoveredPrompt.Store(message.Content)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"recovered"}}]}`)
	}))
	t.Cleanup(server.Close)
	testApp := &interactiveTurnTestApp{
		workspaceRoot: t.TempDir(),
		turnIdle:      make(chan struct{}, 16),
	}
	testApp.app = &app{
		cfg:    config{model: "test-model", maxIterations: 1, workspaceRoot: workspace, toolMaxParallel: 1, toolTimeoutSec: 1},
		client: &client{endpoint: server.URL, model: "test-model", http: server.Client()},
		sink:   &spySink{},
	}
	testApp.app.interactiveIdleHook = func() { testApp.turnIdle <- struct{}{} }
	session := &interactiveSession{messages: []types.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "history"}}}
	pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
	pipe.send("/compact\r")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("compaction did not start")
	}
	pipe.send("draft\r")
	time.Sleep(25 * time.Millisecond)
	if requests.Load() != 1 {
		t.Fatalf("busy draft was queued as request %d", requests.Load())
	}
	pipe.send("\x1b\x1b")
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("compaction was not cancelled")
	}
	testApp.waitForTurn(t)
	pipe.send("\r")
	testApp.waitForTurn(t)
	_ = pipe.Close()
	if err := <-done; err != nil {
		t.Fatalf("compaction cancellation/recovery loop: %v", err)
	}
	if requests.Load() != 2 || containsMessage(session.messages, compactedHistoryMarker) || recoveredPrompt.Load() != "draft" {
		t.Fatalf("cancelled compaction did not recover original history: requests=%d messages=%#v", requests.Load(), session.messages)
	}
}

func containsMessage(messages []contracts.Message, text string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}
