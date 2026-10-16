package app

import (
	"bytes"
	"capelin-go/internal/types"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func recordedInteractiveRequests(testApp *interactiveTurnTestApp) []types.Request {
	testApp.mu.Lock()
	defer testApp.mu.Unlock()
	return append([]types.Request(nil), testApp.requests...)
}

func finishInteractiveSessionAndCaptureStderr(t *testing.T, testApp *interactiveTurnTestApp, session *interactiveSession) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStderr := os.Stderr
	os.Stderr = writer
	testApp.app.finishInteractiveSession(session)
	os.Stderr = previousStderr
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestInteractiveRecapAndPlanCommandsPersistWithoutTools(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("fresh recap", "", nil),
		chatTurnResponse("plan with steps", "", nil),
		chatTurnResponse("follow-up understood", "", nil),
	)
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true, toolReadFile: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	session, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "test"},
		{Role: "user", Content: "previous question"},
		{Role: "assistant", Content: "previous answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
	pipe.send("/recap\r")
	testApp.waitForTurn(t)
	pipe.send("/plan organize the release\r")
	testApp.waitForTurn(t)
	pipe.send("follow up on the plan\r")
	testApp.waitForTurn(t)
	pipe.send("/exit\r")
	if err := <-done; err != nil {
		t.Fatalf("interactive command loop: %v", err)
	}

	requests := recordedInteractiveRequests(testApp)
	if len(requests) != 3 {
		t.Fatalf("provider received %d requests, want recap, plan, and follow-up", len(requests))
	}
	for index, request := range requests[:2] {
		if len(request.Tools) != 0 {
			t.Errorf("command request %d exposed tools: %#v", index+1, request.Tools)
		}
	}
	planPrompt := ""
	recapPrompt := ""
	var hasPreviousQuestion, hasPreviousAnswer bool
	for _, message := range requests[0].Messages {
		if message.Role == "user" && message.Content == "previous question" {
			hasPreviousQuestion = true
		}
		if message.Role == "assistant" && message.Content == "previous answer" {
			hasPreviousAnswer = true
		}
		if message.Role == "user" && strings.Contains(message.Content, "concise recap") {
			recapPrompt = message.Content
		}
	}
	if !strings.Contains(recapPrompt, "concise recap") || !hasPreviousQuestion || !hasPreviousAnswer {
		t.Fatalf("recap request did not include the existing conversation and recap instruction: messages=%#v", requests[0].Messages)
	}
	for _, message := range requests[1].Messages {
		if message.Role == "user" {
			planPrompt = message.Content
		}
	}
	if !strings.Contains(planPrompt, "organize the release") {
		t.Fatalf("plan request omitted the supplied task: %q", planPrompt)
	}
	for _, assistantReply := range []string{"fresh recap", "plan with steps"} {
		found := false
		for _, message := range requests[2].Messages {
			if message.Role == "assistant" && message.Content == assistantReply {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("follow-up request did not retain assistant reply %q: %#v", assistantReply, requests[2].Messages)
		}
	}
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := store.list()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("saved interactive sessions = %d, err=%v", len(sessions), err)
	}
	for _, assistantReply := range []string{"fresh recap", "plan with steps"} {
		found := false
		for _, message := range sessions[0].Messages {
			if message.Role == "assistant" && message.Content == assistantReply {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("saved session did not retain assistant reply %q: %#v", assistantReply, sessions[0].Messages)
		}
	}
}

func TestInteractivePlanCommandPreservesGoalChecklist(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, chatTurnResponse("plan only", "", nil))
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "one", Content: "keep this", Source: "test", Status: todoStatusInProgress}}
	session.activeGoal = &goalState{
		Objective:  "finish the existing objective",
		Generation: 4,
		Completion: &goalCompletion{
			Summary:              "saved progress",
			Evidence:             []string{"the checklist remains current"},
			Generation:           4,
			ChecklistFingerprint: "saved fingerprint",
		},
	}
	syncRuntimeTodos(session)
	originalTodos := cloneTodos(session.todos)
	originalGoal := cloneGoalState(session.activeGoal)
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/plan plan the next step"); stopped {
		t.Fatal("plan command unexpectedly stopped the REPL")
	}
	if !reflect.DeepEqual(session.todos, originalTodos) || !reflect.DeepEqual(session.activeGoal, originalGoal) {
		t.Fatalf("planning changed goal state: todos=%#v goal=%#v", session.todos, session.activeGoal)
	}
	requests := recordedInteractiveRequests(testApp)
	if len(requests) != 1 || len(requests[0].Tools) != 0 {
		t.Fatalf("plan request exposed tools: %#v", requests)
	}
	store, err := testApp.app.ensureSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Todos, originalTodos) || !reflect.DeepEqual(saved.ActiveGoal, originalGoal) {
		t.Fatalf("planning changed persisted goal state: todos=%#v goal=%#v", saved.Todos, saved.ActiveGoal)
	}
}

func TestInteractiveRecapAndPlanFailuresPreserveConversation(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
		command  string
		cancel   bool
		failSave bool
	}{
		{name: "recap provider failure", response: "not-json", command: "/recap"},
		{name: "plan provider failure", response: "not-json", command: "/plan implement the task"},
		{name: "recap cancellation", command: "/recap", cancel: true},
		{name: "plan cancellation", command: "/plan implement the task", cancel: true},
		{name: "recap persistence failure", response: chatTurnResponse("not committed", "", nil), command: "/recap", failSave: true},
		{name: "plan persistence failure", response: chatTurnResponse("not committed", "", nil), command: "/plan implement the task", failSave: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testApp := newInteractiveTurnTestAppWithResponses(t, test.response)
			session, err := testApp.app.newInteractiveSession([]types.Message{
				{Role: "system", Content: "test"},
				{Role: "user", Content: "previous request"},
				{Role: "assistant", Content: "previous answer"},
			})
			if err != nil {
				t.Fatal(err)
			}
			session.lastResponse = "previous answer"
			if err := testApp.app.saveInteractiveSession(session); err != nil {
				t.Fatal(err)
			}
			store, err := testApp.app.ensureSessionStore()
			if err != nil {
				t.Fatal(err)
			}
			path := store.pathFor(session.id)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			originalMessages := cloneMessages(session.messages)
			originalResponse := session.lastResponse
			if test.failSave {
				store.writeAtomic = func(string, []byte) error { return errors.New("injected session write failure") }
			}
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			if stopped := testApp.app.handleInteractiveInput(ctx, session, test.command); stopped {
				t.Fatal("failed command unexpectedly stopped the REPL")
			}
			if !reflect.DeepEqual(session.messages, originalMessages) || session.lastResponse != originalResponse {
				t.Fatalf("failed request changed in-memory conversation: messages=%#v lastResponse=%q", session.messages, session.lastResponse)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("failed request changed saved conversation:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func TestInteractiveRecapAndPlanInvalidUsageStaysLocal(t *testing.T) {
	for _, input := range []string{"/recap extra", "/plan", "/plan   ", "/archive extra"} {
		t.Run(input, func(t *testing.T) {
			testApp := newInteractiveTurnTestApp(t)
			session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			original := cloneMessages(session.messages)
			if stopped := testApp.app.handleInteractiveInput(context.Background(), session, input); stopped {
				t.Fatal("invalid command usage unexpectedly stopped the REPL")
			}
			if got := recordedInteractiveRequests(testApp); len(got) != 0 {
				t.Fatalf("invalid command usage reached the provider: %#v", got)
			}
			if !reflect.DeepEqual(session.messages, original) {
				t.Fatalf("invalid command usage changed conversation: %#v", session.messages)
			}
		})
	}
}

func TestInteractiveArchiveOfStartupResumedArchiveExitsWithoutActivation(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "test"},
		{Role: "user", Content: "preserve this conversation"},
		{Role: "assistant", Content: "archived answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	session.lastResponse = "archived answer"
	session.todos = []todoItem{{ID: "one", Content: "remember checklist", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	archivedID := session.id
	store, err := testApp.app.ensureSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	archivePath := store.archivedPathFor(archivedID)
	if err := store.archive(archivedID); err != nil {
		t.Fatal(err)
	}
	testApp.app.cfg.resumeRequested = true
	testApp.app.cfg.resumeID = archivedID
	resumed, err := testApp.app.startInteractiveSession()
	if err != nil || resumed.id != archivedID {
		t.Fatalf("startup --resume could not select archive: session=%#v err=%v", resumed, err)
	}

	var successMessage string
	testApp.app.sink.(*spySink).onSystem = func(message string) {
		if strings.Contains(message, "archived session "+archivedID) {
			successMessage = message
		}
	}
	pipe, done := startInteractiveSessionPipe(t, testApp, resumed, "")
	pipe.send("/archive\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interactive resumed-archive loop: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = pipe.Close()
		<-done
		t.Fatal("/archive on a resumed archive did not exit interactive mode")
	}
	if resumed.id != archivedID || len(resumed.todos) != 1 || resumed.lastResponse != "archived answer" {
		t.Fatalf("archiving resumed session changed its state: id=%s todos=%#v response=%q", resumed.id, resumed.todos, resumed.lastResponse)
	}
	if !strings.Contains(successMessage, "exiting interactive mode") {
		t.Fatalf("archive success message = %q, want archived ID and exit notice", successMessage)
	}
	if _, err := os.Stat(store.pathFor(archivedID)); !os.IsNotExist(err) {
		t.Fatalf("resumed archive was activated: %v", err)
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("resumed archive is missing: %v", err)
	}
	active, err := store.list()
	if err != nil || len(active) != 0 {
		t.Fatalf("active sessions after archiving resumed archive = %#v err=%v", active, err)
	}
	testApp.app.finishInteractiveSession(resumed)
	if _, err := os.Stat(store.pathFor(archivedID)); !os.IsNotExist(err) {
		t.Fatalf("exit cleanup recreated active snapshot: %v", err)
	}
	if _, err := store.resolve(archivedID[:12]); err != nil {
		t.Fatalf("archived snapshot could not be resumed by unique prefix: %v", err)
	}
}

func TestInteractiveArchiveCommandExitsWithoutCreatingReplacement(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "test"},
		{Role: "user", Content: "finished conversation"},
		{Role: "assistant", Content: "saved final answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "test"},
		{Role: "user", Content: "keep this other session active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	session.lastResponse = "saved final answer"
	originalID := session.id
	store, err := testApp.app.ensureSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	unrelatedPath := store.pathFor(unrelated.id)
	unrelatedBefore, err := os.ReadFile(unrelatedPath)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	store.writeAtomic = func(path string, data []byte) error {
		writes++
		return os.WriteFile(path, data, 0o600)
	}
	var successMessage string
	testApp.app.sink.(*spySink).onSystem = func(message string) {
		if strings.Contains(message, "archived session "+originalID) {
			successMessage = message
		}
	}

	pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
	pipe.send("/archive\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interactive archive loop: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = pipe.Close()
		<-done
		t.Fatal("successful /archive did not exit interactive mode")
	}

	if session.id != originalID {
		t.Fatalf("/archive replaced session %s with %s", originalID, session.id)
	}
	if !strings.Contains(successMessage, "exiting interactive mode") {
		t.Fatalf("archive success message = %q, want archived ID and exit notice", successMessage)
	}
	if strings.Contains(successMessage, "resume with --resume") {
		t.Fatalf("archive confirmation included the generic resume hint: %q", successMessage)
	}
	if _, err := os.Stat(store.pathFor(originalID)); !os.IsNotExist(err) {
		t.Fatalf("archived session remains active after exit cleanup: %v", err)
	}
	if _, err := os.Stat(store.archivedPathFor(originalID)); err != nil {
		t.Fatalf("archive snapshot missing after exit cleanup: %v", err)
	}
	active, err := store.list()
	if err != nil || len(active) != 1 || active[0].SessionUUID != unrelated.id {
		t.Fatalf("active sessions after /archive = %#v err=%v, want only unrelated session %s", active, err, unrelated.id)
	}
	unrelatedAfter, err := os.ReadFile(unrelatedPath)
	if err != nil || !bytes.Equal(unrelatedBefore, unrelatedAfter) {
		t.Fatalf("archiving changed unrelated session snapshot: err=%v", err)
	}
	if writes != 1 {
		t.Fatalf("session persistence writes through REPL shutdown = %d, want only the archive save", writes)
	}
	resolved, err := store.resolve(originalID)
	if err != nil || resolved.SessionUUID != originalID {
		t.Fatalf("archived session could not be resumed by explicit ID: snapshot=%#v err=%v", resolved, err)
	}
	stderr := finishInteractiveSessionAndCaptureStderr(t, testApp, session)
	if strings.Contains(stderr, "resume with --resume") {
		t.Fatalf("outer shutdown printed generic resume hint after archive: %q", stderr)
	}
	if writes != 1 {
		t.Fatalf("outer shutdown saved archived session again: writes=%d", writes)
	}
}

func TestInteractiveExitAndQuitKeepSavingResumeHints(t *testing.T) {
	for _, command := range []string{"/exit", "/quit"} {
		t.Run(command, func(t *testing.T) {
			testApp := newInteractiveTurnTestApp(t)
			session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			session.lastResponse = "saved before ordinary exit"
			store, err := testApp.app.ensureSessionStore()
			if err != nil {
				t.Fatal(err)
			}

			pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
			pipe.send(command + "\r")
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("interactive %s loop: %v", command, err)
				}
			case <-time.After(2 * time.Second):
				_ = pipe.Close()
				<-done
				t.Fatalf("%s did not exit interactive mode", command)
			}

			stderr := finishInteractiveSessionAndCaptureStderr(t, testApp, session)
			if !strings.Contains(stderr, "session "+session.id+"; resume with --resume "+session.id) {
				t.Fatalf("%s shutdown output omitted resume hint: %q", command, stderr)
			}
			snapshot, err := store.resolve(session.id)
			if err != nil || snapshot.LastContent != "saved before ordinary exit" {
				t.Fatalf("%s did not preserve its active snapshot: snapshot=%#v err=%v", command, snapshot, err)
			}
		})
	}
}

func TestInteractiveFallbackArchiveExitsBeforeReadingAnotherPrompt(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]types.Message{
		{Role: "system", Content: "test"},
		{Role: "user", Content: "finished conversation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	originalID := session.id
	store, err := testApp.app.ensureSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := runInteractiveFallbackInput(t, testApp, session, "/archive\nthis prompt must not run\n"); err != nil {
		t.Fatalf("fallback archive loop: %v", err)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("fallback loop read input after /archive: %#v", got)
	}
	if session.id != originalID {
		t.Fatalf("fallback /archive replaced session %s with %s", originalID, session.id)
	}
	active, err := store.list()
	if err != nil || len(active) != 0 {
		t.Fatalf("fallback active sessions after /archive = %#v err=%v, want none", active, err)
	}
	if _, err := os.Stat(store.archivedPathFor(originalID)); err != nil {
		t.Fatalf("fallback archive snapshot missing: %v", err)
	}
}

func runInteractiveFallbackInput(t *testing.T, testApp *interactiveTurnTestApp, session *interactiveSession, input string) error {
	t.Helper()
	previousStdin := os.Stdin
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	os.Stdin = reader
	defer func() {
		os.Stdin = previousStdin
		_ = reader.Close()
	}()
	if _, err := writer.Write([]byte(input)); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return testApp.app.runInteractiveFallbackSession(context.Background(), session)
}

func TestInteractiveFallbackArchiveFailuresKeepInputAvailable(t *testing.T) {
	for _, failure := range []string{"save", "move"} {
		t.Run(failure, func(t *testing.T) {
			testApp := newInteractiveTurnTestApp(t)
			session, err := testApp.app.newInteractiveSession([]types.Message{
				{Role: "system", Content: "test"},
				{Role: "user", Content: "recoverable conversation"},
			})
			if err != nil {
				t.Fatal(err)
			}
			originalID := session.id
			store, err := testApp.app.ensureSessionStore()
			if err != nil {
				t.Fatal(err)
			}
			archivePath := store.archivedPathFor(originalID)
			store.writeAtomic = func(path string, data []byte) error {
				if path == store.pathFor(originalID) && failure == "save" {
					return errors.New("injected archive save failure")
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					return err
				}
				if path == store.pathFor(originalID) && failure == "move" {
					if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
						return err
					}
					return os.WriteFile(archivePath, []byte("pre-existing archive"), 0o600)
				}
				return nil
			}
			archiveError := make(chan string, 1)
			testApp.app.sink.(*spySink).onSystem = func(message string) {
				if strings.Contains(message, "/archive failed:") {
					archiveError <- message
				}
			}

			if err := runInteractiveFallbackInput(t, testApp, session, "/archive\nconfirm input still works\n"); err != nil {
				t.Fatalf("fallback loop after archive %s failure: %v", failure, err)
			}
			select {
			case message := <-archiveError:
				if !strings.Contains(message, "remains available with --resume "+originalID) {
					t.Fatalf("archive error omitted recovery ID: %q", message)
				}
			case <-time.After(time.Second):
				t.Fatalf("archive %s failure was not reported", failure)
			}
			if got := testApp.userPrompts(); !reflect.DeepEqual(got, []string{"confirm input still works"}) {
				t.Fatalf("input after archive %s failure was not processed: %#v", failure, got)
			}
			if session.id != originalID {
				t.Fatalf("archive %s failure replaced session %s with %s", failure, originalID, session.id)
			}
			if _, err := os.Stat(store.pathFor(originalID)); err != nil {
				t.Fatalf("archive %s failure lost active snapshot: %v", failure, err)
			}
		})
	}
}

func TestInteractiveArchiveMoveAndSaveFailuresKeepConversationRecoverable(t *testing.T) {
	t.Run("archive destination conflict", func(t *testing.T) {
		testApp := newInteractiveTurnTestApp(t)
		session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "keep me"}})
		if err != nil {
			t.Fatal(err)
		}
		store, err := testApp.app.ensureSessionStore()
		if err != nil {
			t.Fatal(err)
		}
		originalID := session.id
		originalMessages := cloneMessages(session.messages)
		archivePath := store.archivedPathFor(originalID)
		store.writeAtomic = func(path string, data []byte) error {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return err
			}
			if path == store.pathFor(originalID) {
				if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
					return err
				}
				return os.WriteFile(archivePath, []byte("pre-existing archive"), 0o600)
			}
			return nil
		}
		var diagnostic string
		testApp.app.sink.(*spySink).onSystem = func(message string) { diagnostic = message }
		if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/archive"); stopped {
			t.Fatal("archive failure unexpectedly stopped the REPL")
		}
		if session.id != originalID || !reflect.DeepEqual(session.messages, originalMessages) {
			t.Fatalf("archive move failure changed current conversation: id=%s messages=%#v", session.id, session.messages)
		}
		if !strings.Contains(diagnostic, "remains available with --resume "+originalID) {
			t.Fatalf("archive failure lacked recovery instruction: %q", diagnostic)
		}
		archived, err := os.ReadFile(archivePath)
		if err != nil || string(archived) != "pre-existing archive" {
			t.Fatalf("archive failure changed existing destination: %q err=%v", archived, err)
		}
		if _, err := os.Stat(store.pathFor(originalID)); err != nil {
			t.Fatalf("archive move failure lost active snapshot: %v", err)
		}
	})

	t.Run("save failure keeps the session active", func(t *testing.T) {
		testApp := newInteractiveTurnTestApp(t)
		session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "keep me"}})
		if err != nil {
			t.Fatal(err)
		}
		store, err := testApp.app.ensureSessionStore()
		if err != nil {
			t.Fatal(err)
		}
		originalID := session.id
		originalMessages := cloneMessages(session.messages)
		store.writeAtomic = func(path string, data []byte) error {
			if path == store.pathFor(originalID) {
				return errors.New("injected archive save failure")
			}
			return os.WriteFile(path, data, 0o600)
		}
		var diagnostic string
		testApp.app.sink.(*spySink).onSystem = func(message string) { diagnostic = message }
		if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/archive"); stopped {
			t.Fatal("archive save failure unexpectedly stopped the REPL")
		}
		if session.id != originalID || !reflect.DeepEqual(session.messages, originalMessages) {
			t.Fatalf("archive save failure changed current conversation: id=%s messages=%#v", session.id, session.messages)
		}
		if !strings.Contains(diagnostic, "remains available with --resume "+originalID) {
			t.Fatalf("archive save error lacked recovery instruction: %q", diagnostic)
		}
		if _, err := os.Stat(store.pathFor(originalID)); err != nil {
			t.Fatalf("archive save failure lost active snapshot: %v", err)
		}
		if _, err := os.Stat(store.archivedPathFor(originalID)); !os.IsNotExist(err) {
			t.Fatalf("archive save failure moved snapshot to archive: stat err=%v", err)
		}
	})
}

func TestInteractiveGoalResumeContinuesPersistedObjectiveAndChecklist(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, chatTurnResponse("made a little progress", "", nil))
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 1)
	session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.activeGoal = &goalState{Objective: "persisted objective", Generation: 8}
	session.todos = []todoItem{{ID: "existing", Content: "existing checklist item", Status: todoStatusInProgress}}
	syncRuntimeTodos(session)
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	originalGoal := cloneGoalState(session.activeGoal)
	originalTodos := cloneTodos(session.todos)
	pipe, done := startInteractiveSessionPipe(t, testApp, session, "")
	pipe.send("/goal resume\r")
	testApp.waitForTurn(t)
	pipe.send("/exit\r")
	if err := <-done; err != nil {
		t.Fatalf("explicit goal resume loop: %v", err)
	}
	requests := recordedInteractiveRequests(testApp)
	if len(requests) != 1 {
		t.Fatalf("explicit goal resume made %d provider requests, want 1", len(requests))
	}
	prompt := ""
	for _, message := range requests[0].Messages {
		if message.Role == "user" {
			prompt = message.Content
		}
	}
	if !strings.Contains(prompt, "Continue working toward the objective \"persisted objective\"") {
		t.Fatalf("explicit resume did not continue the persisted objective: %q", prompt)
	}
	if !reflect.DeepEqual(session.activeGoal, originalGoal) || !reflect.DeepEqual(session.todos, originalTodos) {
		t.Fatalf("explicit resume replaced the objective or checklist: goal=%#v todos=%#v", session.activeGoal, session.todos)
	}
}

func TestInteractiveGoalResumeWithoutUnfinishedGoalIsLocal(t *testing.T) {
	completedTodos := []todoItem{{ID: "done", Content: "finished work", Status: todoStatusCompleted}}
	completedGoal := &goalState{
		Objective:  "completed objective",
		Generation: 3,
		Completion: &goalCompletion{
			Summary:              "verified complete",
			Evidence:             []string{"the finished work was checked"},
			Generation:           3,
			ChecklistFingerprint: todosFingerprint(completedTodos),
		},
	}
	for _, test := range []struct {
		name  string
		goal  *goalState
		todos []todoItem
	}{
		{name: "no goal"},
		{name: "goal already complete", goal: completedGoal, todos: completedTodos},
	} {
		t.Run(test.name, func(t *testing.T) {
			testApp := newInteractiveTurnTestApp(t)
			testApp.app.cfg.yolo = true
			session, err := testApp.app.newInteractiveSession([]types.Message{{Role: "system", Content: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			session.activeGoal = cloneGoalState(test.goal)
			session.todos = cloneTodos(test.todos)
			syncRuntimeTodos(session)
			if err := testApp.app.saveInteractiveSession(session); err != nil {
				t.Fatal(err)
			}
			store, err := testApp.app.ensureSessionStore()
			if err != nil {
				t.Fatal(err)
			}
			path := store.pathFor(session.id)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			originalMessages := cloneMessages(session.messages)
			originalGoal := cloneGoalState(session.activeGoal)
			originalTodos := cloneTodos(session.todos)
			var diagnostic string
			testApp.app.sink.(*spySink).onSystem = func(message string) { diagnostic = message }

			if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/goal resume"); stopped {
				t.Fatal("no-goal resume unexpectedly stopped the REPL")
			}
			if got := recordedInteractiveRequests(testApp); len(got) != 0 {
				t.Fatalf("no-goal resume reached the provider: %#v", got)
			}
			if !strings.Contains(diagnostic, "no unfinished goal to resume") || !strings.Contains(diagnostic, "/goal <objective>") {
				t.Fatalf("no-goal response did not explain how to start a goal: %q", diagnostic)
			}
			if !reflect.DeepEqual(session.messages, originalMessages) || !reflect.DeepEqual(session.activeGoal, originalGoal) || !reflect.DeepEqual(session.todos, originalTodos) {
				t.Fatalf("no-goal resume changed current state: messages=%#v goal=%#v todos=%#v", session.messages, session.activeGoal, session.todos)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("no-goal resume changed saved snapshot:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}
