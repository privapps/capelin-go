package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/output"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type goalHeartbeatEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *goalHeartbeatEventRecorder) record(message string) {
	r.mu.Lock()
	r.events = append(r.events, message)
	r.mu.Unlock()
}

func (r *goalHeartbeatEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func captureGoalHeartbeatEvents(testApp *interactiveTurnTestApp) *goalHeartbeatEventRecorder {
	recorder := &goalHeartbeatEventRecorder{}
	testApp.app.goalHeartbeatInitialDelay = time.Millisecond
	testApp.app.goalHeartbeatCadence = time.Millisecond
	testApp.app.sink.(*spySink).onSystem = recorder.record
	return recorder
}

func assertGoalHeartbeatLifecycle(t *testing.T, recorder *goalHeartbeatEventRecorder, terminal string) {
	t.Helper()
	events := recorder.snapshot()
	if len(events) == 0 {
		t.Fatal("goal produced no system events")
	}
	terminalIndex := -1
	heartbeatSeen := false
	for index, event := range events {
		if strings.Contains(event, " working;") {
			heartbeatSeen = true
		}
		if strings.Contains(event, terminal) {
			if terminalIndex == -1 {
				terminalIndex = index
			}
		}
	}
	if !heartbeatSeen {
		t.Fatalf("goal produced no heartbeat before terminal status: %v", events)
	}
	if terminalIndex == -1 {
		t.Fatalf("goal did not produce terminal status containing %q: %v", terminal, events)
	}
	if terminalIndex != len(events)-1 {
		t.Fatalf("terminal status was not the last goal event: %v", events)
	}

	eventCount := len(events)
	time.Sleep(30 * time.Millisecond)
	if after := recorder.snapshot(); len(after) != eventCount {
		t.Fatalf("heartbeat emitted after runGoal returned: %v", after[eventCount:])
	}
}

// assertGoalHeartbeatSummaryContract asserts the stable recurring heartbeat
// contract introduced by the compact summary: the "[goal] " prefix and the
// " working;" lifecycle marker are preserved, total elapsed time is present,
// and the fields appear in the order todo count -> explicitly named current
// todo -> agent summary. It also rejects the previous ambiguous bare
// "current:" label and the verbose per-state subagent breakdown, which now
// lives only in the ::agents view.
func assertGoalHeartbeatSummaryContract(t *testing.T, message string) {
	t.Helper()
	if !strings.HasPrefix(message, "[goal] iteration ") || !strings.Contains(message, " working;") {
		t.Fatalf("heartbeat lost its prefix or lifecycle marker: %q", message)
	}
	if !strings.Contains(message, "; total elapsed ") {
		t.Fatalf("heartbeat omitted total elapsed time: %q", message)
	}
	todosIndex := strings.Index(message, "; todos ")
	currentIndex := strings.Index(message, "; current todo")
	agentsIndex := strings.Index(message, "; subagents ")
	if todosIndex < 0 || currentIndex < 0 || agentsIndex < 0 {
		t.Fatalf("heartbeat missing todo/current-todo/agent fields (todos=%d current=%d agents=%d): %q", todosIndex, currentIndex, agentsIndex, message)
	}
	if todosIndex >= currentIndex || currentIndex >= agentsIndex {
		t.Fatalf("heartbeat fields out of order (todos=%d current=%d agents=%d): %q", todosIndex, currentIndex, agentsIndex, message)
	}
	if strings.Contains(message, "; current: ") {
		t.Fatalf("heartbeat still exposes an unlabeled current field: %q", message)
	}
	if strings.Contains(message, "states pending=") {
		t.Fatalf("heartbeat still repeats the ::agents state breakdown: %q", message)
	}
}

func newBlockedGoalHeartbeatApp(t *testing.T) (*app, *interactiveSession, <-chan struct{}, chan struct{}, <-chan string) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	events := make(chan string, 32)
	var startedOnce sync.Once
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(chatTurnResponse("done", "", nil))),
				Request:    r,
			}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	a := &app{
		cfg: config{
			model:             "test-model",
			maxIterations:     1,
			maxGoalIterations: 1,
			workspaceRoot:     t.TempDir(),
			toolMaxParallel:   1,
			toolTimeoutSec:    1,
			yolo:              true,
		},
		client: &client{
			endpoint: "http://goal.test/chat/completions",
			model:    "test-model",
			http:     &http.Client{Transport: transport},
		},
		sink: &spySink{onSystem: func(message string) { events <- message }},
	}
	session, err := a.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	return a, session, started, release, events
}

func waitForGoalHeartbeatEvent(t *testing.T, events <-chan string, want string) string {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if strings.Contains(event, want) {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for goal event containing %q", want)
		}
	}
}

func TestInteractiveGoalHeartbeatReportsBlockedTurnAndStopsBeforeTerminalStatus(t *testing.T) {
	a, session, started, release, events := newBlockedGoalHeartbeatApp(t)
	a.goalHeartbeatInitialDelay = 10 * time.Millisecond
	a.goalHeartbeatCadence = 10 * time.Millisecond

	done := make(chan bool, 1)
	go func() { done <- a.runGoal(context.Background(), session, "blocked objective") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal provider request did not start")
	}

	initial := waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	assertGoalHeartbeatSummaryContract(t, initial)
	if !strings.Contains(initial, "total elapsed") || !strings.Contains(initial, "current turn elapsed") ||
		!strings.Contains(initial, "todos 0/0 completed; current todo: none; subagents 0 active") {
		t.Fatalf("immediate goal status omitted elapsed-time fields: %q", initial)
	}
	subsequent := waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	assertGoalHeartbeatSummaryContract(t, subsequent)
	if !strings.Contains(subsequent, "current turn elapsed") ||
		!strings.Contains(subsequent, "todos 0/0 completed; current todo: none; subagents 0 active") {
		t.Fatalf("subsequent heartbeat omitted current-turn elapsed time: %q", subsequent)
	}

	close(release)
	select {
	case stopped := <-done:
		if stopped {
			t.Fatal("completed blocked goal unexpectedly stopped the session")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked goal did not finish after provider release")
	}
	terminal := waitForGoalHeartbeatEvent(t, events, "[goal] incomplete")
	if strings.Contains(terminal, "working") {
		t.Fatalf("terminal goal status was not terminal: %q", terminal)
	}
	select {
	case event := <-events:
		t.Fatalf("heartbeat emitted after runGoal returned: %q", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestResumedGoalStartsFreshHeartbeatWithCurrentProfileAndRestoresOrdinaryRuntime(t *testing.T) {
	a, session, started, release, events := newBlockedGoalHeartbeatApp(t)
	a.goalHeartbeatInitialDelay = 10 * time.Millisecond
	a.goalHeartbeatCadence = 10 * time.Millisecond
	ordinary, goal := testRuntimeProfiles(1, 2, 64, 1, 2)
	a.cfg.ordinaryProfile = ordinary
	a.cfg.goalProfile = goal
	a.cfg.profilesResolved = true

	session.messages = []contracts.Message{{Role: "system", Content: "test"}}
	session.activeGoal = &goalState{Objective: "resume the unfinished objective", Generation: 7}
	session.todos = []todoItem{{ID: "work", Content: "unfinished work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	if err := a.saveInteractiveSession(session); err != nil {
		t.Fatalf("save incomplete goal: %v", err)
	}
	raw, err := os.ReadFile(a.sessionStore.pathFor(session.id))
	if err != nil {
		t.Fatalf("read saved incomplete goal: %v", err)
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{`"heartbeat"`, `"timer"`, `"reporter"`} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("snapshot persisted reporter state %q: %s", forbidden, raw)
		}
	}

	snapshot, err := a.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatalf("resolve incomplete goal: %v", err)
	}
	resumed := a.sessionFromSnapshot(snapshot)
	if resumed.activeGoal == nil || resumed.activeGoal.Objective != session.activeGoal.Objective || resumed.activeGoal.Generation != session.activeGoal.Generation {
		t.Fatalf("resumed active goal changed: got %#v, want %#v", resumed.activeGoal, session.activeGoal)
	}
	if len(resumed.todos) != 1 || resumed.todos[0].Status != todoStatusPending {
		t.Fatalf("resumed checklist changed: %#v", resumed.todos)
	}
	if resumed.runtime.executionProfile != ordinary {
		t.Fatalf("resumed session did not start with ordinary profile: got=%+v want=%+v", resumed.runtime.executionProfile, ordinary)
	}

	var releaseOnce sync.Once
	releaseProvider := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseProvider)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan bool, 1)
	go func() { done <- a.runGoal(ctx, resumed, "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("resumed goal provider request did not start")
	}
	heartbeat := waitForGoalHeartbeatEvent(t, events, "iteration 1/64 working")
	if !strings.Contains(heartbeat, "total elapsed") || !strings.Contains(heartbeat, "current turn elapsed") {
		t.Fatalf("fresh resumed heartbeat omitted elapsed-time fields: %q", heartbeat)
	}

	cancel()
	select {
	case stopped := <-done:
		if !stopped {
			t.Fatal("cancelled resumed goal did not stop the session")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled resumed goal did not return")
	}
	releaseProvider()
	terminal := waitForGoalHeartbeatEvent(t, events, "[goal] incomplete")
	if !strings.Contains(terminal, "cancelled") || strings.Contains(terminal, "working") {
		t.Fatalf("unexpected resumed cancellation status: %q", terminal)
	}
	select {
	case event := <-events:
		t.Fatalf("resumed heartbeat emitted after runGoal returned: %q", event)
	case <-time.After(30 * time.Millisecond):
	}
	if resumed.runtime.executionProfile != ordinary || resumed.runtime.maxToolIterations != ordinary.MaxIterations || resumed.runtime.goalIsEnabled() {
		t.Fatalf("resumed goal did not restore ordinary runtime: profile=%+v iterations=%d enabled=%v", resumed.runtime.executionProfile, resumed.runtime.maxToolIterations, resumed.runtime.goalIsEnabled())
	}
	if stopped := a.runInteractiveTurn(context.Background(), resumed, "ordinary follow-up"); stopped {
		t.Fatal("ordinary follow-up after resumed goal unexpectedly stopped the session")
	}
	if resumed.runtime.executionProfile != ordinary || resumed.runtime.maxToolIterations != ordinary.MaxIterations || resumed.runtime.goalIsEnabled() {
		t.Fatalf("ordinary runtime changed after resumed follow-up: profile=%+v iterations=%d enabled=%v", resumed.runtime.executionProfile, resumed.runtime.maxToolIterations, resumed.runtime.goalIsEnabled())
	}
}

func TestInteractiveGoalHeartbeatReportsOrderedCurrentChecklistAndActiveSubagents(t *testing.T) {
	a, session, started, release, events := newBlockedGoalHeartbeatApp(t)
	a.goalHeartbeatInitialDelay = 10 * time.Millisecond
	a.goalHeartbeatCadence = 10 * time.Millisecond
	session.activeGoal = &goalState{Objective: "resume objective", Generation: 1}
	session.todos = []todoItem{
		{ID: "first", Content: " first  item ", Status: todoStatusInProgress},
		{ID: "second", Content: "completed item", Status: todoStatusCompleted},
		{ID: "third", Content: "third\nitem", Status: todoStatusInProgress},
	}
	syncRuntimeTodos(session)
	a.cfg.allowedTools = map[string]bool{toolListFiles: true}
	a.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), nil)
	if _, err := a.subagents.create(context.Background(), a.rootRuntime(), createSubagentArgs{Question: "pending child"}); err != nil {
		t.Fatalf("create pending subagent: %v", err)
	}

	done := make(chan bool, 1)
	go func() { done <- a.runGoal(context.Background(), session, "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal provider request did not start")
	}

	heartbeat := waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	assertGoalHeartbeatSummaryContract(t, heartbeat)
	wantProgress := "todos 1/3 completed; current todos: first item (+1 more); subagents 1 active"
	if !strings.Contains(heartbeat, wantProgress) {
		t.Fatalf("heartbeat omitted ordered runtime progress: got %q, want substring %q", heartbeat, wantProgress)
	}
	if strings.Contains(heartbeat, "third item") {
		t.Fatalf("heartbeat joined every in-progress todo instead of previewing the first: %q", heartbeat)
	}

	close(release)
	select {
	case stopped := <-done:
		if stopped {
			t.Fatal("completed blocked goal unexpectedly stopped the session")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked goal did not finish after provider release")
	}
	_ = waitForGoalHeartbeatEvent(t, events, "[goal] incomplete")
	select {
	case event := <-events:
		t.Fatalf("heartbeat emitted after runGoal returned: %q", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestInteractiveGoalHeartbeatStopsBeforeCancellationStatus(t *testing.T) {
	a, session, started, release, events := newBlockedGoalHeartbeatApp(t)
	a.goalHeartbeatInitialDelay = 10 * time.Millisecond
	a.goalHeartbeatCadence = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- a.runGoal(ctx, session, "cancelled objective") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal provider request did not start")
	}
	_ = waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	cancel()
	select {
	case stopped := <-done:
		if !stopped {
			t.Fatal("cancelled goal did not stop the session")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled goal did not return")
	}
	close(release)
	terminal := waitForGoalHeartbeatEvent(t, events, "[goal] incomplete")
	if !strings.Contains(terminal, "cancelled") || strings.Contains(terminal, "working") {
		t.Fatalf("unexpected cancellation status: %q", terminal)
	}
	select {
	case event := <-events:
		t.Fatalf("heartbeat emitted after cancellation returned: %q", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestInteractiveGoalHeartbeatStopsBeforeSuccessfulStatus(t *testing.T) {
	completed := goalTodosArguments("verified work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("todo", toolUpdateTodos, completed)}),
		chatTurnResponse("progress", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the final state was verified"]}`)}),
		chatTurnResponse("done", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	testApp.app.goalHeartbeatInitialDelay = time.Hour
	testApp.app.goalHeartbeatCadence = time.Hour
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { events = append(events, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.runGoal(context.Background(), session, "complete this objective"); stopped {
		t.Fatal("successful goal unexpectedly stopped the session")
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1], "[goal] complete after") {
		t.Fatalf("successful terminal status was not last: %v", events)
	}
	for _, event := range events[:len(events)-1] {
		if strings.Contains(event, "[goal] complete after") {
			t.Fatalf("successful terminal status was followed by another goal event: %v", events)
		}
	}
	eventCount := len(events)
	time.Sleep(20 * time.Millisecond)
	if len(events) != eventCount {
		t.Fatalf("heartbeat emitted after successful runGoal returned: %v", events[eventCount:])
	}
}

func TestInteractiveGoalHeartbeatStopsBeforeProviderFailureStatus(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	t.Cleanup(provider.Close)
	testApp.app.client.endpoint = provider.URL
	testApp.app.client.http = provider.Client()
	recorder := captureGoalHeartbeatEvents(testApp)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.runGoal(context.Background(), session, "provider failure"); stopped {
		t.Fatal("provider failure unexpectedly stopped the session")
	}
	assertGoalHeartbeatLifecycle(t, recorder, "provider or tool failure")
}

func TestInteractiveGoalHeartbeatStopsBeforeProviderOrToolFailureStatus(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	firstResponse := chatTurnResponse("", "", []map[string]any{
		goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work", "pending")),
	})
	requests := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			_, _ = w.Write([]byte(firstResponse))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"upstream failed after tool execution"}`))
	}))
	t.Cleanup(provider.Close)
	testApp.app.client.endpoint = provider.URL
	testApp.app.client.http = provider.Client()
	recorder := captureGoalHeartbeatEvents(testApp)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.runGoal(context.Background(), session, "tool failure"); stopped {
		t.Fatal("provider/tool failure unexpectedly stopped the session")
	}
	if requests != 2 {
		t.Fatalf("provider requests=%d, want tool turn followed by failure", requests)
	}
	assertGoalHeartbeatLifecycle(t, recorder, "provider or tool failure")
}

func TestInteractiveGoalHeartbeatStopsBeforePersistenceFailureStatus(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, chatTurnResponse("progress", "", nil))
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 2)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	testApp.app.sessionStore.writeAtomic = func(path string, data []byte) error {
		writes++
		if writes >= 3 {
			return errors.New("session disk unavailable")
		}
		return os.WriteFile(path, data, 0o600)
	}
	recorder := captureGoalHeartbeatEvents(testApp)

	if stopped := testApp.app.runGoal(context.Background(), session, "persistence failure"); stopped {
		t.Fatal("persistence failure unexpectedly stopped the session")
	}
	if writes < 2 {
		t.Fatalf("persistence failure did not occur after heartbeat start: writes=%d", writes)
	}
	assertGoalHeartbeatLifecycle(t, recorder, "session persistence failure")
}

func TestInteractiveGoalHeartbeatStopsBeforeStalledStatus(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("still working", "", nil),
		chatTurnResponse("still working", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 3)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	session.activeGoal = &goalState{Objective: "stalled objective", Generation: 1}
	session.todos = []todoItem{{ID: "work", Content: "unfinished work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	recorder := captureGoalHeartbeatEvents(testApp)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("stalled goal unexpectedly stopped the session")
	}
	assertGoalHeartbeatLifecycle(t, recorder, "stalled after")
}

func TestInteractiveGoalHeartbeatStopsBeforeIterationLimitStatus(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("todo", toolUpdateTodos, goalTodosArguments("unfinished", "pending")),
		}),
		chatTurnResponse("progress", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 1)
	recorder := captureGoalHeartbeatEvents(testApp)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.runGoal(context.Background(), session, "iteration bound"); stopped {
		t.Fatal("iteration-limited goal unexpectedly stopped the session")
	}
	assertGoalHeartbeatLifecycle(t, recorder, "iteration limit reached")
}

func TestGoalHeartbeatFormattingHandlesElapsedTimeAndStopIdempotently(t *testing.T) {
	var messages []string
	heartbeat := newGoalHeartbeat(func(message string) { messages = append(messages, message) }, 64, time.Hour, time.Hour)
	heartbeat.startedAt = time.Unix(100, 0)
	heartbeat.now = func() time.Time { return time.Unix(103, 0) }
	heartbeat.beginIteration(2)
	heartbeat.stop()
	heartbeat.stop()
	heartbeat.beginIteration(3)

	if len(messages) != 1 || messages[0] != "[goal] iteration 2/64 working; total elapsed 03s; current turn elapsed 00s; todos 0/0 completed; current todo: none; subagents 0 active" {
		t.Fatalf("unexpected deterministic heartbeat message: %#v", messages)
	}
	assertGoalHeartbeatSummaryContract(t, messages[0])
	if heartbeat.started {
		t.Fatal("heartbeat unexpectedly restarted after stop")
	}
}

func TestGoalHeartbeatUsesCapturedGoalStartTime(t *testing.T) {
	var messages []string
	heartbeat := newGoalHeartbeatAt(time.Unix(100, 0), func(message string) {
		messages = append(messages, message)
	}, 1, time.Hour, time.Hour)
	heartbeat.now = func() time.Time { return time.Unix(103, 0) }
	heartbeat.beginIteration(1)
	heartbeat.stop()

	if len(messages) != 1 || !strings.Contains(messages[0], "total elapsed 03s") {
		t.Fatalf("heartbeat did not use captured goal start time: %#v", messages)
	}
}

func TestGoalHeartbeatDefaultsToThirtySecondRecurringCadence(t *testing.T) {
	heartbeat := newGoalHeartbeat(func(string) {}, 8, 0, 0)
	if heartbeat.initialDelay != goalHeartbeatInitialDelay {
		t.Fatalf("zero initial delay did not fall back to %s: got %s", goalHeartbeatInitialDelay, heartbeat.initialDelay)
	}
	if heartbeat.cadence != goalHeartbeatCadence {
		t.Fatalf("zero cadence did not fall back to the %s default: got %s", goalHeartbeatCadence, heartbeat.cadence)
	}
	if goalHeartbeatCadence != 30*time.Second {
		t.Fatalf("goalHeartbeatCadence constant = %s, want 30s", goalHeartbeatCadence)
	}
}

func TestGoalHeartbeatProgressCountsUseOneSnapshotPerHeartbeat(t *testing.T) {
	var messages []string
	todoSnapshots := 0
	agentSnapshots := 0
	heartbeat := newGoalHeartbeat(func(message string) { messages = append(messages, message) }, 8, time.Hour, time.Hour, goalHeartbeatProgress{
		todosSnapshot: func() []todoItem {
			todoSnapshots++
			return []todoItem{
				{ID: "one", Content: "one", Status: todoStatusCompleted},
				{ID: "two", Content: "two", Status: todoStatusPending},
				{ID: "three", Content: " three\nitem ", Status: todoStatusInProgress},
				{ID: "four", Content: "four", Status: todoStatusCancelled},
				{ID: "five", Content: "five", Status: todoStatusInProgress},
			}
		},
		agentSnapshot: func() []contracts.SubagentNode {
			agentSnapshots++
			return []contracts.SubagentNode{
				{Status: string(subagentStatusPending)},
				{Status: string(subagentStatusQueued)},
				{Status: string(subagentStatusRunning)},
				{Status: string(subagentStatusCompleted)},
				{Status: string(subagentStatusFailed)},
				{Status: string(subagentStatusCancelled)},
				{Status: string(subagentStatusTimedOut)},
			}
		},
	})
	heartbeat.startedAt = time.Unix(100, 0)
	heartbeat.now = func() time.Time { return time.Unix(162, 0) }
	heartbeat.beginIteration(2)
	heartbeat.stop()

	if len(messages) != 1 {
		t.Fatalf("expected one heartbeat, got %d: %#v", len(messages), messages)
	}
	want := "[goal] iteration 2/8 working; total elapsed 1m02s; current turn elapsed 00s; todos 1/5 completed; current todos: three item (+1 more); subagents 3 active"
	if messages[0] != want {
		t.Fatalf("unexpected progress heartbeat: %q", messages[0])
	}
	assertGoalHeartbeatSummaryContract(t, messages[0])
	if todoSnapshots != len(messages) || agentSnapshots != len(messages) {
		t.Fatalf("expected one snapshot per heartbeat, got todos=%d agents=%d heartbeats=%d", todoSnapshots, agentSnapshots, len(messages))
	}
}

func TestGoalHeartbeatFormatsSingleCurrentTodo(t *testing.T) {
	var messages []string
	heartbeat := newGoalHeartbeat(
		func(message string) { messages = append(messages, message) },
		1,
		time.Hour,
		time.Hour,
		goalHeartbeatProgress{
			todosSnapshot: func() []todoItem {
				return []todoItem{
					{ID: "only", Content: "  verify   output  ", Status: todoStatusInProgress},
				}
			},
			agentSnapshot: func() []contracts.SubagentNode { return nil },
		},
	)
	heartbeat.startedAt = time.Unix(0, 0)
	heartbeat.now = func() time.Time { return time.Unix(3, 0) }
	heartbeat.beginIteration(1)
	heartbeat.stop()

	want := "[goal] iteration 1/1 working; total elapsed 03s; current turn elapsed 00s; todos 0/1 completed; current todo: verify output; subagents 0 active"
	if len(messages) != 1 || messages[0] != want {
		t.Fatalf("unexpected single-current heartbeat: got %#v, want %q", messages, want)
	}
	assertGoalHeartbeatSummaryContract(t, messages[0])
}

func newDeterministicGoalHeartbeat(t *testing.T, messages *[]string, todos []todoItem, agents []contracts.SubagentNode) *goalHeartbeat {
	t.Helper()
	heartbeat := newGoalHeartbeatAt(
		time.Unix(100, 0),
		func(message string) { *messages = append(*messages, message) },
		4,
		time.Hour,
		time.Hour,
		goalHeartbeatProgress{
			todosSnapshot: func() []todoItem { return todos },
			agentSnapshot: func() []contracts.SubagentNode { return agents },
		},
	)
	heartbeat.now = func() time.Time { return time.Unix(103, 0) }
	return heartbeat
}

// TestGoalHeartbeatOrdersTodoCountBeforeCurrentTodoBeforeAgents pins acceptance
// criterion 1: todo count, then the named current todo, then the agents field.
func TestGoalHeartbeatOrdersTodoCountBeforeCurrentTodoBeforeAgents(t *testing.T) {
	var messages []string
	heartbeat := newDeterministicGoalHeartbeat(t, &messages, []todoItem{
		{ID: "done", Content: "done work", Status: todoStatusCompleted},
		{ID: "active", Content: "active work", Status: todoStatusInProgress},
	}, []contracts.SubagentNode{{Status: string(subagentStatusRunning)}})
	heartbeat.beginIteration(1)
	heartbeat.stop()

	if len(messages) != 1 {
		t.Fatalf("expected one heartbeat, got %#v", messages)
	}
	assertGoalHeartbeatSummaryContract(t, messages[0])
	want := "todos 1/2 completed; current todo: active work; subagents 1 active"
	if !strings.Contains(messages[0], want) {
		t.Fatalf("heartbeat ordering changed: got %q, want substring %q", messages[0], want)
	}
}

// TestGoalHeartbeatBoundsLongCurrentTodoPreview pins acceptance criterion 3:
// the current-todo value is whitespace-normalized and truncated through the
// shared output.Preview helper instead of being emitted verbatim.
func TestGoalHeartbeatBoundsLongCurrentTodoPreview(t *testing.T) {
	t.Cleanup(func() { output.SetPreviewMax(0) })
	output.SetPreviewMax(40)
	long := "alpha " + strings.Repeat("beta\tgamma\n", 40)
	var messages []string
	heartbeat := newDeterministicGoalHeartbeat(t, &messages, []todoItem{
		{ID: "long", Content: long, Status: todoStatusInProgress},
	}, nil)
	heartbeat.beginIteration(1)
	heartbeat.stop()

	if len(messages) != 1 {
		t.Fatalf("expected one heartbeat, got %#v", messages)
	}
	message := messages[0]
	assertGoalHeartbeatSummaryContract(t, message)
	if strings.Contains(message, "\n") || strings.Contains(message, "\t") {
		t.Fatalf("heartbeat did not normalize todo whitespace: %q", message)
	}
	if !strings.Contains(message, "more chars)") {
		t.Fatalf("heartbeat did not bound the long todo preview: %q", message)
	}
	if strings.Contains(message, long) {
		t.Fatalf("heartbeat emitted the unbounded todo text: %q", message)
	}
	preview := message[strings.Index(message, "; current todo: ")+len("; current todo: ") : strings.Index(message, "; subagents ")]
	if len([]rune(preview)) > 40 {
		t.Fatalf("current todo preview exceeded the preview budget (%d runes): %q", len([]rune(preview)), preview)
	}
}

// TestGoalHeartbeatSummarizesMultipleInProgressTodos pins acceptance criterion
// 3 for the multi-item case: the first preview plus an additional-item count,
// never a full join of every in-progress item.
func TestGoalHeartbeatSummarizesMultipleInProgressTodos(t *testing.T) {
	var messages []string
	heartbeat := newDeterministicGoalHeartbeat(t, &messages, []todoItem{
		{ID: "one", Content: " first   item ", Status: todoStatusInProgress},
		{ID: "two", Content: "second item", Status: todoStatusInProgress},
		{ID: "three", Content: "third item", Status: todoStatusInProgress},
	}, nil)
	heartbeat.beginIteration(2)
	heartbeat.stop()

	if len(messages) != 1 {
		t.Fatalf("expected one heartbeat, got %#v", messages)
	}
	message := messages[0]
	assertGoalHeartbeatSummaryContract(t, message)
	if !strings.Contains(message, "current todos: first item (+2 more); subagents 0 active") {
		t.Fatalf("heartbeat did not summarize multiple in-progress todos: %q", message)
	}
	for _, hidden := range []string{"second item", "third item"} {
		if strings.Contains(message, hidden) {
			t.Fatalf("heartbeat exposed additional in-progress todo %q: %q", hidden, message)
		}
	}
}

// TestGoalHeartbeatReportsExplicitEmptyCurrentTodo pins acceptance criterion 4:
// no in-progress todo renders an explicit, singular, todo-scoped empty value
// that is distinguishable from the agent summary.
func TestGoalHeartbeatReportsExplicitEmptyCurrentTodo(t *testing.T) {
	var messages []string
	heartbeat := newDeterministicGoalHeartbeat(t, &messages, []todoItem{
		{ID: "one", Content: "done work", Status: todoStatusCompleted},
		{ID: "two", Content: "waiting work", Status: todoStatusPending},
	}, []contracts.SubagentNode{{Status: string(subagentStatusRunning)}, {Status: string(subagentStatusCompleted)}})
	heartbeat.beginIteration(1)
	heartbeat.stop()

	if len(messages) != 1 {
		t.Fatalf("expected one heartbeat, got %#v", messages)
	}
	message := messages[0]
	assertGoalHeartbeatSummaryContract(t, message)
	if !strings.Contains(message, "todos 1/2 completed; current todo: none; subagents 1 active") {
		t.Fatalf("heartbeat did not report an explicit empty current todo: %q", message)
	}
	if strings.Contains(message, "current todos:") {
		t.Fatalf("empty current todo used the plural label: %q", message)
	}
}

// TestGoalHeartbeatRetainsIterationAndElapsedFields pins acceptance criterion
// 5: iteration, total elapsed, and current-turn elapsed survive the compaction,
// and the current-turn field disappears only once the turn ends.
func TestGoalHeartbeatRetainsIterationAndElapsedFields(t *testing.T) {
	var messages []string
	heartbeat := newDeterministicGoalHeartbeat(t, &messages, nil, nil)
	heartbeat.beginIteration(3)
	heartbeat.emit()
	heartbeat.endTurn()
	heartbeat.emit()
	heartbeat.stop()

	if len(messages) != 3 {
		t.Fatalf("expected three heartbeats, got %#v", messages)
	}
	for _, message := range messages {
		assertGoalHeartbeatSummaryContract(t, message)
		if !strings.HasPrefix(message, "[goal] iteration 3/4 working; total elapsed 03s") {
			t.Fatalf("heartbeat lost iteration or total elapsed time: %q", message)
		}
	}
	for _, message := range messages[:2] {
		if !strings.Contains(message, "; current turn elapsed 00s;") {
			t.Fatalf("active-turn heartbeat lost current-turn elapsed time: %q", message)
		}
	}
	if strings.Contains(messages[2], "current turn elapsed") {
		t.Fatalf("heartbeat reported current-turn elapsed time after the turn ended: %q", messages[2])
	}
}

// TestGoalHeartbeatBlankCurrentTodoDoesNotRenderNone pins the blank-todo
// collision fix: a real in-progress todo whose content is blank or
// whitespace-only renders the explicit "(blank)" placeholder and must never
// collapse into "none", which is reserved for having no in-progress todo.
func TestGoalHeartbeatBlankCurrentTodoDoesNotRenderNone(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "whitespace", content: " \t\n  "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var messages []string
			heartbeat := newDeterministicGoalHeartbeat(t, &messages, []todoItem{
				{ID: "blank", Content: testCase.content, Status: todoStatusInProgress},
			}, nil)
			heartbeat.beginIteration(1)
			heartbeat.stop()

			if len(messages) != 1 {
				t.Fatalf("expected one heartbeat, got %#v", messages)
			}
			message := messages[0]
			assertGoalHeartbeatSummaryContract(t, message)
			if !strings.Contains(message, "current todo: (blank)") {
				t.Fatalf("blank in-progress todo did not render the blank placeholder: %q", message)
			}
			if strings.Contains(message, "current todo: none") {
				t.Fatalf("blank in-progress todo collided with the no-current-todo state: %q", message)
			}
			if strings.Contains(message, "current todos:") {
				t.Fatalf("single blank current todo used the plural label: %q", message)
			}
		})
	}
}

func TestFormatGoalHeartbeatDurationPadsSecondsAcrossUnits(t *testing.T) {
	tests := []struct {
		name  string
		value time.Duration
		want  string
	}{
		{name: "zero", value: 0, want: "00s"},
		{name: "seconds", value: 3 * time.Second, want: "03s"},
		{name: "minute", value: time.Minute + 3*time.Second, want: "1m03s"},
		{name: "hour", value: time.Hour + 2*time.Minute + 3*time.Second, want: "1h2m03s"},
		{name: "rounded", value: 1500 * time.Millisecond, want: "02s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatGoalHeartbeatDuration(test.value); got != test.want {
				t.Fatalf("formatGoalHeartbeatDuration(%s) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}
