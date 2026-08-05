package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompleteGoalValidatesArgumentsAndIsGoalOnly(t *testing.T) {
	app := &app{cfg: config{allowedTools: map[string]bool{toolCompleteGoal: true}}}
	call := func(arguments string) contracts.ToolCall {
		return contracts.ToolCall{Function: contracts.FunctionCall{Name: toolCompleteGoal, Arguments: arguments}}
	}
	ordinary := &agentRuntime{allowedTools: map[string]bool{toolCompleteGoal: true}}
	if _, err := app.runToolForRuntime(context.Background(), ordinary, call(`{"summary":"done","evidence":["verified"]}`)); err == nil {
		t.Fatal("ordinary runtime exposed complete_goal")
	}
	goalWithoutPermission := &agentRuntime{}
	goalWithoutPermission.enableGoal(1)
	if _, err := app.runToolForRuntime(context.Background(), goalWithoutPermission, call(`{"summary":"done","evidence":["verified"]}`)); err == nil {
		t.Fatal("goal runtime without the goal-only permission executed complete_goal")
	}

	invalid := []string{
		`{}`,
		`{"summary":"done","evidence":[]}`,
		`{"summary":"done","evidence":[" "]}`,
		`{"summary":"done","evidence":"verified"}`,
		`{"summary":"done","evidence":["verified"],"extra":true}`,
		`{"summary":"done","evidence":["verified"]}{}`,
	}
	for _, raw := range invalid {
		runtime := &agentRuntime{allowedTools: map[string]bool{toolCompleteGoal: true}}
		runtime.enableGoal(1)
		if _, err := app.runToolForRuntime(context.Background(), runtime, call(raw)); err == nil {
			t.Fatalf("malformed completion claim unexpectedly succeeded: %s", raw)
		}
		if runtime.snapshotGoalClaim() != nil {
			t.Fatalf("malformed claim was recorded: %s", raw)
		}
	}

	runtime := &agentRuntime{allowedTools: map[string]bool{toolCompleteGoal: true}}
	runtime.enableGoal(7)
	result, err := app.runToolForRuntime(context.Background(), runtime, call(`{"summary":"  done  ","evidence":["  verified  "]}`))
	if err != nil || !strings.Contains(result, `"recorded": true`) {
		t.Fatalf("valid claim failed: result=%q err=%v", result, err)
	}
	claim := runtime.snapshotGoalClaim()
	if claim == nil || claim.Summary != "done" || claim.Evidence[0] != "verified" || claim.Generation != 7 {
		t.Fatalf("valid claim was not normalized: %#v", claim)
	}
	runtime.replaceTodos([]todoItem{{ID: "one", Content: "work", Status: todoStatusCompleted}})
	if runtime.snapshotGoalClaim() != nil {
		t.Fatal("checklist replacement did not invalidate completion claim")
	}
}

func TestCompleteGoalIsNotAdvertisedInOrdinaryToolCatalogs(t *testing.T) {
	for _, tool := range buildAgentTools(map[string]bool{
		toolUpdateTodos:  true,
		toolCompleteGoal: true,
	}) {
		if tool.Function.Name == toolCompleteGoal {
			t.Fatal("ordinary tool catalog advertised goal-only complete_goal")
		}
	}
}

func TestCompleteGoalConcurrentChecklistInvalidationIsSafe(t *testing.T) {
	application := &app{cfg: config{allowedTools: map[string]bool{toolCompleteGoal: true}}}
	runtime := &agentRuntime{allowedTools: map[string]bool{toolCompleteGoal: true}}
	runtime.enableGoal(1)
	completedTodos := []todoItem{{ID: "work", Content: "work", Status: todoStatusCompleted}}
	runtime.replaceTodos(completedTodos)
	call := contracts.ToolCall{Function: contracts.FunctionCall{
		Name:      toolCompleteGoal,
		Arguments: `{"summary":"done","evidence":["verified"]}`,
	}}
	if _, err := application.runToolForRuntime(context.Background(), runtime, call); err != nil {
		t.Fatalf("initial completion claim failed: %v", err)
	}
	runtime.replaceTodos(completedTodos)
	if claim := runtime.snapshotGoalClaim(); claim != nil {
		t.Fatalf("identical checklist replacement retained a stale completion claim: %#v", claim)
	}

	var panics atomic.Int32
	var waitGroup sync.WaitGroup
	for i := 0; i < 8; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for j := 0; j < 2000; j++ {
				func() {
					defer func() {
						if recover() != nil {
							panics.Add(1)
						}
					}()
					_, _ = application.runToolForRuntime(context.Background(), runtime, call)
				}()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for j := 0; j < 2000; j++ {
				runtime.replaceTodos([]todoItem{{ID: "work", Content: "work", Status: todoStatusPending}})
				runtime.replaceTodos([]todoItem{{ID: "work", Content: "work", Status: todoStatusCompleted}})
			}
		}()
	}
	waitGroup.Wait()

	if got := panics.Load(); got != 0 {
		t.Fatalf("concurrent checklist invalidation caused %d complete_goal panic(s)", got)
	}
	runtime.replaceTodos([]todoItem{{ID: "work", Content: "work", Status: todoStatusPending}})
	if claim := runtime.snapshotGoalClaim(); claim != nil {
		t.Fatalf("checklist invalidation retained a completion claim: %#v", claim)
	}
	state := &goalState{Objective: "work", Generation: 1, Completion: runtime.snapshotGoalClaim()}
	if validGoalCompletion(state, runtime.snapshotTodos()) {
		t.Fatal("an invalidated completion claim satisfied the goal predicate")
	}
}

func newInteractiveGoalCancellationHarness(t *testing.T) (*interactiveTurnTestApp, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	pending := goalTodosArguments("unfinished work", "pending")
	completed := goalTodosArguments("finished work", "completed")
	responses := []string{
		chatTurnResponse("", "", []map[string]any{goalToolCall("seed", toolUpdateTodos, pending)}),
		chatTurnResponse("started", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("finish", toolUpdateTodos, completed)}),
		chatTurnResponse("verified", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the final checklist was verified"]}`)}),
		chatTurnResponse("done", "", nil),
	}
	harness := &interactiveTurnTestApp{workspaceRoot: t.TempDir(), turnIdle: make(chan struct{}, 16)}
	requestCount := atomic.Int32{}
	responseIndex := 0
	firstBlocked := make(chan struct{})
	firstCancelled := make(chan struct{})
	var blockedOnce sync.Once
	var cancelledOnce sync.Once
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		index := int(requestCount.Add(1))
		var request types.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		harness.mu.Lock()
		harness.requests = append(harness.requests, request)
		harness.mu.Unlock()
		if index == 3 {
			blockedOnce.Do(func() { close(firstBlocked) })
			<-r.Context().Done()
			cancelledOnce.Do(func() { close(firstCancelled) })
			return nil, r.Context().Err()
		}
		if responseIndex >= len(responses) {
			return nil, errors.New("unexpected goal provider request")
		}
		response := responses[responseIndex]
		responseIndex++
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(response)),
			Request:    r,
		}, nil
	})
	harness.app = &app{
		cfg: config{
			model:             "test-model",
			maxIterations:     1,
			maxGoalIterations: 3,
			workspaceRoot:     harness.workspaceRoot,
			toolMaxParallel:   1,
			toolTimeoutSec:    1,
			yolo:              true,
			allowedTools:      map[string]bool{toolUpdateTodos: true},
		},
		client: &client{endpoint: "http://goal.test/chat/completions", model: "test-model", http: &http.Client{Transport: transport}},
		sink:   &spySink{},
	}
	harness.app.toolset = buildAgentTools(harness.app.cfg.allowedTools)
	harness.app.interactiveIdleHook = func() { harness.turnIdle <- struct{}{} }
	return harness, firstBlocked, firstCancelled
}

func startCancellableInteractiveGoal(t *testing.T, harness *interactiveTurnTestApp, session *interactiveSession, objective string) *interactiveTurnController {
	t.Helper()
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		harness.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !harness.app.startInteractiveTurn(context.Background(), controller, session, func(ctx context.Context, worker *interactiveSession) (bool, error) {
		return harness.app.runGoal(ctx, worker, objective), nil
	}) {
		t.Fatal("failed to start cancellable goal turn")
	}
	return controller
}

func TestInteractiveGoalCancellationCommitsWorkerForBareResume(t *testing.T) {
	harness, blocked, cancelled := newInteractiveGoalCancellationHarness(t)
	session, err := harness.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	controller := startCancellableInteractiveGoal(t, harness, session, "finish the work")
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("goal did not reach its cancellable continuation")
	}
	if !controller.cancelActive() {
		t.Fatal("controller did not report an active goal turn")
	}
	controller.wait()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("goal provider request was not cancelled")
	}
	if stopped := harness.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("bare goal resume unexpectedly stopped the session")
	}
	if got := len(harness.userPrompts()); got != 7 {
		t.Fatalf("bare /goal did not resume the cancelled worker: provider calls=%d, want 7", got)
	}
	if session.activeGoal == nil || session.activeGoal.Objective != "finish the work" || !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("resumed goal did not retain and complete the active worker state: goal=%#v todos=%#v", session.activeGoal, session.todos)
	}
}

func TestInteractiveGoalCancellationSurvivesControllerShutdown(t *testing.T) {
	harness, blocked, cancelled := newInteractiveGoalCancellationHarness(t)
	session, err := harness.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	controller := startCancellableInteractiveGoal(t, harness, session, "preserve this work")
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("goal did not reach its cancellable continuation")
	}
	controller.shutdown()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("goal provider request was not cancelled")
	}
	if err := harness.app.saveInteractiveSession(session); err != nil {
		t.Fatalf("shutdown session save: %v", err)
	}
	snapshot, err := harness.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveGoal == nil || snapshot.ActiveGoal.Objective != "preserve this work" {
		t.Fatalf("shutdown overwrote the cancelled active goal: %#v", snapshot.ActiveGoal)
	}
	if len(snapshot.Todos) != 1 || snapshot.Todos[0].Status != todoStatusPending {
		t.Fatalf("shutdown did not preserve the resumable checklist: %#v", snapshot.Todos)
	}
}

func TestInteractiveOrdinaryCancellationDoesNotCommitGoalMetadataWorker(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	session.activeGoal = &goalState{Objective: "already finished objective", Generation: 2}
	session.todos = []todoItem{{ID: "work", Content: "work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	session.lastResponse = "previous response"
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		worker.lastResponse = "cancelled ordinary response"
		worker.todos = []todoItem{{ID: "work", Content: "work", Status: todoStatusCompleted}}
		return true, context.Canceled
	}) {
		t.Fatal("ordinary turn did not start")
	}
	controller.wait()
	if session.lastResponse != "previous response" || len(session.todos) != 1 || session.todos[0].Status != todoStatusPending {
		t.Fatalf("ordinary cancellation committed worker state: response=%q todos=%#v", session.lastResponse, session.todos)
	}
}

func runInteractiveGoalCommand(t *testing.T, testApp *interactiveTurnTestApp, session *interactiveSession, ctx context.Context, input string) {
	t.Helper()
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if stopped := testApp.app.handleInteractiveInputAsync(ctx, controller, session, nil, nil, input); stopped {
		t.Fatalf("goal command %q unexpectedly ended the interactive session", input)
	}
	controller.wait()
}

func TestInteractiveGoalCommandContinuesAfterInvalidCompletionClaim(t *testing.T) {
	completed := goalTodosArguments("finished work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("invalid", toolCompleteGoal, `{"summary":" ","evidence":[" "]}`),
			goalToolCall("todo", toolUpdateTodos, completed),
		}),
		chatTurnResponse("progress", "", nil),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the final checklist was verified"]}`),
		}),
		chatTurnResponse("done", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	var toolErrors []string
	testApp.app.sink.(*spySink).onToolResult = func(name string, isError bool, detail string) {
		if isError {
			toolErrors = append(toolErrors, name+":"+detail)
		}
	}
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { events = append(events, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	runInteractiveGoalCommand(t, testApp, session, context.Background(), "/goal recover invalid claim")
	if len(toolErrors) == 0 || !strings.Contains(toolErrors[0], "complete_goal") {
		t.Fatalf("invalid completion claim did not use the recoverable tool path: %v", toolErrors)
	}
	if len(testApp.userPrompts()) != 4 || session.activeGoal == nil || !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("invalid claim prevented the scripted goal recovery: prompts=%#v goal=%#v todos=%#v", testApp.userPrompts(), session.activeGoal, session.todos)
	}
	if !containsEvent(events, "[goal] complete after") {
		t.Fatalf("recovered goal did not expose a success outcome: %v", events)
	}
}

func TestInteractiveGoalCommandInvalidatesClaimAfterIdenticalChecklistReplacement(t *testing.T) {
	completed := goalTodosArguments("finished work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("todo", toolUpdateTodos, completed)}),
		chatTurnResponse("progress", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("stale", toolCompleteGoal, `{"summary":"stale","evidence":["the first final state was checked"]}`)}),
		chatTurnResponse("", "", []map[string]any{goalToolCall("replace", toolUpdateTodos, completed)}),
		chatTurnResponse("rechecking", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("fresh", toolCompleteGoal, `{"summary":"finished","evidence":["the replacement checklist was checked"]}`)}),
		chatTurnResponse("done", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 2, 3)
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { events = append(events, message) }
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	runInteractiveGoalCommand(t, testApp, session, context.Background(), "/goal replace the final state")
	if len(testApp.userPrompts()) != 7 || session.activeGoal == nil || !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("stale claim was accepted or fresh claim failed: prompts=%#v goal=%#v todos=%#v", testApp.userPrompts(), session.activeGoal, session.todos)
	}
	if !containsEvent(events, "handshake is missing or stale") {
		t.Fatalf("stale-claim continuation was not visible: %v", events)
	}
}

func TestBareGoalResumesPersistedEmptyChecklistAfterCancellation(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("todo", toolUpdateTodos, goalTodosArguments("fresh work", "completed"))}),
		chatTurnResponse("checklist established", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["fresh checklist verified"]}`)}),
		chatTurnResponse("done", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.activeGoal = &goalState{Objective: "recover the empty goal", Generation: 9}
	session.todos = nil
	syncRuntimeTodos(session)
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runInteractiveGoalCommand(t, testApp, session, ctx, "/goal")
	if session.activeGoal == nil || session.activeGoal.Objective != "recover the empty goal" || session.activeGoal.Generation != 9 || len(session.todos) != 0 {
		t.Fatalf("cancelled empty goal did not retain its persisted identity: goal=%#v todos=%#v", session.activeGoal, session.todos)
	}
	snapshot, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveGoal == nil || snapshot.ActiveGoal.Objective != "recover the empty goal" || snapshot.ActiveGoal.Generation != 9 || len(snapshot.Todos) != 0 {
		t.Fatalf("cancelled empty goal was not persisted for recovery: %#v todos=%#v", snapshot.ActiveGoal, snapshot.Todos)
	}
	runInteractiveGoalCommand(t, testApp, session, context.Background(), "/goal")
	if len(testApp.userPrompts()) != 4 || session.activeGoal.Generation != 9 || !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("bare /goal did not establish the persisted empty goal: prompts=%#v goal=%#v todos=%#v", testApp.userPrompts(), session.activeGoal, session.todos)
	}
}

func TestGoalCompletionPersistsObjectiveSummaryEvidenceAndFinalChecklist(t *testing.T) {
	completed := goalTodosArguments("verified work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("todo", toolUpdateTodos, completed)}),
		chatTurnResponse("progress", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"finished the objective","evidence":["the implementation passed verification"]}`)}),
		chatTurnResponse("finished", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 3)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runGoal(context.Background(), session, "persist this objective"); stopped {
		t.Fatal("successful goal unexpectedly stopped the REPL")
	}
	snapshot, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveGoal == nil || snapshot.ActiveGoal.Objective != "persist this objective" || snapshot.ActiveGoal.Generation != 1 {
		t.Fatalf("objective metadata was not persisted: %#v", snapshot.ActiveGoal)
	}
	if snapshot.ActiveGoal.Completion == nil || snapshot.ActiveGoal.Completion.Summary != "finished the objective" || len(snapshot.ActiveGoal.Completion.Evidence) != 1 {
		t.Fatalf("completion metadata was not persisted: %#v", snapshot.ActiveGoal)
	}
	if !todosComplete(snapshot.Todos) || snapshot.ActiveGoal.Completion.ChecklistFingerprint != todosFingerprint(session.todos) {
		t.Fatalf("persisted completion was not tied to final checklist: todos=%#v claim=%#v", snapshot.Todos, snapshot.ActiveGoal.Completion)
	}
}

func TestBareGoalResumesPersistedObjectiveAndLegacyCompletedTodosNeedHandshake(t *testing.T) {
	t.Run("persisted active objective", func(t *testing.T) {
		testApp := newInteractiveTurnTestAppWithResponses(t,
			chatTurnResponse("", "", []map[string]any{
				goalToolCall("todo", toolUpdateTodos, goalTodosArguments("finished", "completed")),
				goalToolCall("claim", toolCompleteGoal, `{"summary":"resumed","evidence":["final state verified"]}`),
			}),
			chatTurnResponse("resumed", "", nil),
		)
		configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
		session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		session.activeGoal = &goalState{Objective: "original objective", Generation: 4}
		session.todos = []todoItem{{ID: "work", Content: "work", Status: todoStatusPending}}
		syncRuntimeTodos(session)
		if err := testApp.app.saveInteractiveSession(session); err != nil {
			t.Fatal(err)
		}
		resumed := testApp.app.sessionFromSnapshot(sessionSnapshot{SessionUUID: session.id, Messages: session.messages, Todos: session.todos, ActiveGoal: session.activeGoal})
		if stopped := testApp.app.runGoal(context.Background(), resumed, ""); stopped {
			t.Fatal("persisted objective resume unexpectedly stopped the REPL")
		}
		prompts := testApp.userPrompts()
		if len(prompts) != 2 || !strings.Contains(prompts[0], `"original objective"`) || resumed.activeGoal.Generation != 4 {
			t.Fatalf("resume did not remain tied to persisted objective: prompts=%#v goal=%#v", prompts, resumed.activeGoal)
		}
	})

	t.Run("legacy completed checklist", func(t *testing.T) {
		testApp := newInteractiveTurnTestAppWithResponses(t,
			chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"legacy resumed","evidence":["checklist was verified"]}`)}),
			chatTurnResponse("resumed", "", nil),
		)
		configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
		session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		session.todos = []todoItem{{ID: "legacy", Content: "legacy work", Status: todoStatusCompleted}}
		if err := testApp.app.saveInteractiveSession(session); err != nil {
			t.Fatal(err)
		}
		snapshot, err := testApp.app.sessionStore.resolve(session.id)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.ActiveGoal != nil {
			t.Fatal("legacy fixture unexpectedly had active goal metadata")
		}
		legacy := testApp.app.sessionFromSnapshot(snapshot)
		if stopped := testApp.app.runGoal(context.Background(), legacy, ""); stopped {
			t.Fatal("legacy recovery unexpectedly stopped the REPL")
		}
		if len(testApp.userPrompts()) != 2 || legacy.activeGoal == nil || !validGoalCompletion(legacy.activeGoal, legacy.todos) {
			t.Fatalf("legacy completed checklist bypassed handshake: prompts=%#v goal=%#v", testApp.userPrompts(), legacy.activeGoal)
		}
	})
}

func TestNewGoalInvalidatesPreviousCompletionBeforeProvider(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 1)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.activeGoal = &goalState{Objective: "old objective", Generation: 3, Completion: &goalCompletion{Summary: "old", Evidence: []string{"old evidence"}, Generation: 3, ChecklistFingerprint: todosFingerprint([]todoItem{{ID: "old", Content: "old", Status: todoStatusCompleted}})}}
	session.todos = []todoItem{{ID: "old", Content: "old", Status: todoStatusCompleted}}
	syncRuntimeTodos(session)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if stopped := testApp.app.runGoal(ctx, session, "new objective"); !stopped {
		t.Fatal("cancelled new objective did not report cancellation")
	}
	if session.activeGoal == nil || session.activeGoal.Objective != "new objective" || session.activeGoal.Generation != 4 || session.activeGoal.Completion != nil || len(session.todos) != 0 {
		t.Fatalf("new objective retained stale completion state: goal=%#v todos=%#v", session.activeGoal, session.todos)
	}
	if len(testApp.userPrompts()) != 0 {
		t.Fatalf("cancelled new objective reached provider: %#v", testApp.userPrompts())
	}
}

func TestLegacySnapshotWithoutGoalMetadataStillLoads(t *testing.T) {
	store, err := newSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.create([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	path := store.pathFor(snapshot.SessionUUID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "activeGoal")
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.resolve(snapshot.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveGoal != nil {
		t.Fatalf("legacy snapshot gained goal metadata while loading: %#v", loaded.ActiveGoal)
	}
}

func TestGoalPersistenceFailureDoesNotReportSuccess(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 1)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	testApp.app.sessionStore.writeAtomic = func(string, []byte) error { return errors.New("disk unavailable") }
	if stopped := testApp.app.runGoal(context.Background(), session, "cannot persist"); stopped {
		t.Fatal("persistence failure unexpectedly stopped the REPL")
	}
	if len(testApp.userPrompts()) != 0 {
		t.Fatalf("persistence failure reached provider: %#v", testApp.userPrompts())
	}
}

func TestGoalCancelledChecklistAndIterationLimitRemainIncomplete(t *testing.T) {
	t.Run("cancelled checklist", func(t *testing.T) {
		testApp := newInteractiveTurnTestAppWithResponses(t,
			chatTurnResponse("", "", []map[string]any{goalToolCall("cancel", toolUpdateTodos, goalTodosArguments("stopped", "cancelled"))}),
			chatTurnResponse("cancelled", "", nil),
		)
		configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
		var events []string
		testApp.app.sink.(*spySink).onSystem = func(message string) { events = append(events, message) }
		session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		if stopped := testApp.app.runGoal(context.Background(), session, "cancel this objective"); stopped {
			t.Fatal("cancelled checklist unexpectedly stopped the REPL")
		}
		if !containsEvent(events, "checklist item \"work\" was cancelled") && !containsEvent(events, "checklist item \"stopped\" was cancelled") {
			t.Fatalf("cancelled checklist reason missing: %v", events)
		}
	})

	t.Run("iteration limit", func(t *testing.T) {
		testApp := newInteractiveTurnTestAppWithResponses(t,
			chatTurnResponse("", "", []map[string]any{goalToolCall("pending", toolUpdateTodos, goalTodosArguments("unfinished", "pending"))}),
			chatTurnResponse("progress", "", nil),
		)
		configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 1)
		var events []string
		testApp.app.sink.(*spySink).onSystem = func(message string) { events = append(events, message) }
		session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		if stopped := testApp.app.runGoal(context.Background(), session, "bounded objective"); stopped {
			t.Fatal("iteration limit unexpectedly stopped the REPL")
		}
		if !containsEvent(events, "iteration limit reached") || session.activeGoal == nil || session.activeGoal.Objective != "bounded objective" {
			t.Fatalf("iteration limit outcome was not resumable: events=%v goal=%#v", events, session.activeGoal)
		}
	})
}
