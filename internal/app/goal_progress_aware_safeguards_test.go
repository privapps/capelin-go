package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestGoalRecoverableErrorThenSuccessfulActivityContinues proves that an
// iteration containing a recoverable tool failure and at least one successful
// non-control tool result is not an error-only recovery turn, and that
// successful inspection/edit/verification work without a checklist change is
// not a stall. Three such iterations would exhaust the recovery budget and two
// would exhaust the stall budget under the old guards.
func TestGoalRecoverableErrorThenSuccessfulActivityContinues(t *testing.T) {
	writeFileArgs := `{"path":"note.txt","content":"verified work"}`
	responses := []string{
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("bad", toolReadFile, `{"path":"missing.txt"}`),
			goalToolCall("inspect", toolListFiles, `{"path":"."}`),
		}),
		chatTurnResponse("inspected", "", nil),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("bad", toolReadFile, `{"path":"missing.txt"}`),
			goalToolCall("edit", toolWriteFile, writeFileArgs),
		}),
		chatTurnResponse("edited", "", nil),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("bad", toolReadFile, `{"path":"missing.txt"}`),
			goalToolCall("verify", toolExecuteProgram, `{"command":"go","args":["version"]}`),
		}),
		chatTurnResponse("verified", "", nil),
	}
	responses = append(responses, goalCompletionResponses()...)
	testApp := newInteractiveTurnTestAppWithResponses(t, responses...)
	configureGoalTestApp(testApp, map[string]bool{
		toolReadFile: true, toolListFiles: true, toolWriteFile: true,
		toolExecuteProgram: true, toolUpdateTodos: true,
	}, 1, 5)
	events := goalSystemEvents(testApp)
	session := goalTestSession(t, testApp, true)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("activity-aware goal unexpectedly stopped the REPL")
	}
	if !todosComplete(session.todos) || !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("goal did not continue to completion: todos=%#v goal=%#v", session.todos, session.activeGoal)
	}
	if got := len(testApp.userPrompts()); got != 8 {
		t.Fatalf("provider calls=%d, want 8", got)
	}
	if containsEvent(*events, "recovery limit reached") || containsEvent(*events, "stalled after") {
		t.Fatalf("successful activity did not reset the safeguards: %v", *events)
	}
}

// TestGoalChecklistChangeResetsRecoveryStreakDespiteErrors proves that a
// changed authoritative checklist is progress even when the same iteration
// also contains a recoverable tool error: the recovery streak must reset
// instead of reaching the three-turn bound.
func TestGoalChecklistChangeResetsRecoveryStreakDespiteErrors(t *testing.T) {
	responses := make([]string, 0, maxConsecutiveGoalRecoveries*2+2)
	for i := 1; i <= maxConsecutiveGoalRecoveries; i++ {
		responses = append(responses,
			chatTurnResponse("", "", []map[string]any{
				goalToolCall("bad", "unknown_tool", `{}`),
				goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work "+string(rune('0'+i)), "pending")),
			}),
			chatTurnResponse("still working", "", nil),
		)
	}
	responses = append(responses, goalCompletionResponses()...)
	testApp := newInteractiveTurnTestAppWithResponses(t, responses...)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 5)
	events := goalSystemEvents(testApp)
	session := goalTestSession(t, testApp, true)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("checklist-progress goal unexpectedly stopped the REPL")
	}
	if !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("goal did not complete: todos=%#v goal=%#v", session.todos, session.activeGoal)
	}
	if containsEvent(*events, "recovery limit reached") {
		t.Fatalf("checklist change did not reset the recovery streak: %v", *events)
	}
}

// TestGoalFailedCommandsDoNotCountAsActivity proves that a non-zero command
// result never sets the successful-activity signal: three consecutive
// iterations that only fail commands still stop with the bounded recovery
// outcome.
func TestGoalFailedCommandsDoNotCountAsActivity(t *testing.T) {
	responses := make([]string, 0, maxConsecutiveGoalRecoveries*2)
	for i := 1; i <= maxConsecutiveGoalRecoveries; i++ {
		responses = append(responses,
			chatTurnResponse("", "", []map[string]any{
				goalToolCall("bad", toolExecuteProgram, `{"command":"go","args":["tool","definitely-not-a-tool"]}`),
			}),
			chatTurnResponse("still failing", "", nil),
		)
	}
	testApp := newInteractiveTurnTestAppWithResponses(t, responses...)
	configureGoalTestApp(testApp, map[string]bool{toolExecuteProgram: true}, 1, 10)
	events := goalSystemEvents(testApp)
	session := goalTestSession(t, testApp, true)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("failed-command goal unexpectedly stopped the REPL")
	}
	if !containsEvent(*events, "recovery limit reached") {
		t.Fatalf("failed commands were treated as activity: %v", *events)
	}
	if got := len(testApp.userPrompts()); got != maxConsecutiveGoalRecoveries*2 {
		t.Fatalf("provider calls=%d, want bounded %d", got, maxConsecutiveGoalRecoveries*2)
	}
}

// TestGoalControlPlaneCallsDoNotResetRecoveryGuard proves that unchanged
// checklist-management replacements and invalid completion-handshake attempts
// never set the activity signal: repeated error-only turns still exhaust the
// bounded recovery budget.
func TestGoalControlPlaneCallsDoNotResetRecoveryGuard(t *testing.T) {
	responses := make([]string, 0, maxConsecutiveGoalRecoveries*2)
	for i := 1; i <= maxConsecutiveGoalRecoveries; i++ {
		responses = append(responses,
			chatTurnResponse("", "", []map[string]any{
				goalToolCall("invalid", toolCompleteGoal, `{"summary":" ","evidence":[" "]}`),
				goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work", "pending")),
			}),
			chatTurnResponse("still working", "", nil),
		)
	}
	testApp := newInteractiveTurnTestAppWithResponses(t, responses...)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 10)
	events := goalSystemEvents(testApp)
	session := goalTestSession(t, testApp, true)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("control-plane goal unexpectedly stopped the REPL")
	}
	if !containsEvent(*events, "recovery limit reached") {
		t.Fatalf("control-plane calls reset the recovery guard: %v", *events)
	}
	if got := len(testApp.userPrompts()); got != maxConsecutiveGoalRecoveries*2 {
		t.Fatalf("provider calls=%d, want bounded %d", got, maxConsecutiveGoalRecoveries*2)
	}
}

// TestGoalAlternatingErrorAndNoOpTurnsStayBounded proves that alternating
// error-only and no-op iterations cannot evade both safeguards: the stall
// streak is preserved across error-only turns, so the goal stops with the
// stalled outcome at the fourth iteration instead of running to the outer
// iteration limit.
func TestGoalAlternatingErrorAndNoOpTurnsStayBounded(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("bad", "unknown_tool", `{}`),
		}),
		chatTurnResponse("still working", "", nil),
		chatTurnResponse("still working", "", nil),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("bad", "unknown_tool", `{}`),
		}),
		chatTurnResponse("still working", "", nil),
		chatTurnResponse("still working", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 10)
	events := goalSystemEvents(testApp)
	session := goalTestSession(t, testApp, true)

	if stopped := testApp.app.runGoal(context.Background(), session, ""); stopped {
		t.Fatal("alternating goal unexpectedly stopped the REPL")
	}
	terminal := goalLastIncompleteTerminal(*events)
	if !strings.Contains(terminal, "stalled after 4 iteration(s)") {
		t.Fatalf("alternating turns were not bounded by the stall guard: %q", terminal)
	}
	if got := len(testApp.userPrompts()); got != 6 {
		t.Fatalf("provider calls=%d, want 6 (error-only: 2 calls each; no-op: 1 call each)", got)
	}
}

// TestGoalSuccessfulActivityAloneNeverCompletes proves that a completed
// checklist plus successful tool activity is still progress, not success: the
// completion handshake remains required and the goal continues.
func TestGoalSuccessfulActivityAloneNeverCompletes(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work", "completed")),
			goalToolCall("verify", toolListFiles, `{"path":"."}`),
		}),
		chatTurnResponse("verified final state", "", nil),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the final checklist is complete"]}`),
		}),
		chatTurnResponse("done", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolListFiles: true, toolUpdateTodos: true}, 1, 3)
	events := goalSystemEvents(testApp)
	session := goalTestSession(t, testApp, false)

	if stopped := testApp.app.runGoal(context.Background(), session, "verify and finish"); stopped {
		t.Fatal("activity-only goal unexpectedly stopped the REPL")
	}
	if !containsEvent(*events, "handshake is missing or stale") {
		t.Fatalf("completed checklist without handshake did not continue explicitly: %v", *events)
	}
	if !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("goal did not complete after the handshake: goal=%#v todos=%#v", session.activeGoal, session.todos)
	}
	successSeen := false
	for _, event := range *events {
		if strings.Contains(event, "[goal] complete after") {
			successSeen = true
		}
	}
	if !successSeen {
		t.Fatalf("goal never reported success: %v", *events)
	}
}

// TestGoalActivityIsNotPersisted proves that the transient per-iteration
// activity signal and safeguard counters never reach persisted session state:
// existing snapshots keep loading without migration.
func TestGoalActivityIsNotPersisted(t *testing.T) {
	responses := []string{
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("inspect", toolListFiles, `{"path":"."}`),
			goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work", "pending")),
		}),
		chatTurnResponse("inspected", "", nil),
	}
	responses = append(responses, goalCompletionResponses()...)
	testApp := newInteractiveTurnTestAppWithResponses(t, responses...)
	configureGoalTestApp(testApp, map[string]bool{toolListFiles: true, toolUpdateTodos: true}, 1, 2)
	session := goalTestSession(t, testApp, false)

	if stopped := testApp.app.runGoal(context.Background(), session, "persist only durable state"); stopped {
		t.Fatal("persistence goal unexpectedly stopped the REPL")
	}
	snapshot, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveGoal == nil || !validGoalCompletion(snapshot.ActiveGoal, snapshot.Todos) {
		t.Fatalf("completed goal was not persisted: goal=%#v todos=%#v", snapshot.ActiveGoal, snapshot.Todos)
	}
	raw, err := json.Marshal(snapshot.ActiveGoal)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		switch key {
		case "objective", "generation", "completion":
		default:
			t.Fatalf("persisted goal state gained transient field %q: %s", key, raw)
		}
	}
	path := testApp.app.sessionStore.pathFor(session.id)
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"goalToolActivity", "recoveryStreak", "unchangedStreak", "\"activity\""} {
		if strings.Contains(string(persisted), marker) {
			t.Fatalf("persisted session contains transient safeguard state %q", marker)
		}
	}
	// A snapshot that predates the feature still loads as before.
	loaded := testApp.app.sessionFromSnapshot(snapshot)
	if loaded.activeGoal == nil || loaded.activeGoal.Objective != "persist only durable state" {
		t.Fatalf("resumed snapshot lost durable goal metadata: %#v", loaded.activeGoal)
	}
	if _, err := json.Marshal(loaded.activeGoal); err != nil {
		t.Fatalf("resumed goal state is not serializable: %v", err)
	}
}

// TestGoalResumeGuidanceIsExplicit proves that the bounded and cancellable
// terminal outcomes carry the resume-versus-fresh-start guidance required by
// the user-facing contract, while non-resumable failure outcomes stay
// explicit without it.
func TestGoalResumeGuidanceIsExplicit(t *testing.T) {
	noOpIteration := chatTurnResponse("still working", "", nil)
	tests := []struct {
		name         string
		responses    []string
		maxGoal      int
		reason       string
		objective    string
		seedTodos    bool
		cancelBefore bool
	}{
		{name: "recovery limit", reason: "recovery limit reached",
			responses: goalErrorOnlyResponses(maxConsecutiveGoalRecoveries),
			maxGoal:   10, seedTodos: true},
		{name: "stalled", reason: "stalled after",
			responses: []string{noOpIteration, noOpIteration},
			maxGoal:   3, seedTodos: true},
		{name: "iteration limit", reason: "iteration limit reached",
			responses: []string{noOpIteration}, maxGoal: 1, seedTodos: true},
		{name: "cancelled", reason: "cancelled after",
			responses: []string{}, maxGoal: 3, objective: "cancel this objective", cancelBefore: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testApp := newInteractiveTurnTestAppWithResponses(t, test.responses...)
			configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, test.maxGoal)
			events := goalSystemEvents(testApp)
			session := goalTestSession(t, testApp, test.seedTodos)
			ctx := context.Background()
			if test.cancelBefore {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_ = testApp.app.runGoal(ctx, session, test.objective)
			terminal := goalLastIncompleteTerminal(*events)
			if terminal == "" {
				t.Fatalf("no incomplete terminal outcome: %v", *events)
			}
			if !strings.Contains(terminal, test.reason) {
				t.Fatalf("terminal %q lacks reason %q", terminal, test.reason)
			}
			if !strings.Contains(terminal, "bare /goal") || !strings.Contains(terminal, "/goal <objective>") {
				t.Fatalf("terminal status lacks resume guidance: %q", terminal)
			}
		})
	}
}

// TestGoalFailureOutcomesDoNotClaimResumeGuidance proves that persistence,
// provider, and fatal failure outcomes remain explicit and incomplete without
// the resume guidance, since their durable state is not guaranteed.
func TestGoalFailureOutcomesDoNotClaimResumeGuidance(t *testing.T) {
	tests := []struct {
		name      string
		responses []string
		reason    string
		configure func(*interactiveTurnTestApp)
	}{
		{name: "persistence failure", reason: "session persistence failure",
			responses: []string{chatTurnResponse("still working", "", nil)},
			configure: func(testApp *interactiveTurnTestApp) {
				testApp.app.sessionStore.writeAtomic = func(string, []byte) error { return errors.New("disk unavailable") }
			}},
		{name: "provider failure", reason: "provider or tool failure",
			responses: []string{},
			configure: func(testApp *interactiveTurnTestApp) {
				testApp.app.client.endpoint = "http://127.0.0.1:1/unreachable"
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testApp := newInteractiveTurnTestAppWithResponses(t, test.responses...)
			configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 3)
			session := goalTestSession(t, testApp, false)
			// Configure failure seams after the session store exists.
			if test.configure != nil {
				test.configure(testApp)
			}
			events := goalSystemEvents(testApp)
			_ = testApp.app.runGoal(context.Background(), session, "failure outcome")
			terminal := goalLastIncompleteTerminal(*events)
			if !strings.Contains(terminal, test.reason) {
				t.Fatalf("terminal %q lacks reason %q", terminal, test.reason)
			}
			if strings.Contains(terminal, "bare /goal") {
				t.Fatalf("failure outcome claims resumability: %q", terminal)
			}
		})
	}
}

func goalTestSession(t *testing.T, testApp *interactiveTurnTestApp, seed bool) *interactiveSession {
	t.Helper()
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if seed {
		session.todos = []todoItem{{ID: "work", Content: "work", Status: todoStatusPending}}
		syncRuntimeTodos(session)
	}
	return session
}

func goalSystemEvents(testApp *interactiveTurnTestApp) *[]string {
	events := new([]string)
	testApp.app.sink.(*spySink).onSystem = func(message string) { *events = append(*events, message) }
	return events
}

func goalErrorOnlyResponses(iterations int) []string {
	responses := make([]string, 0, iterations*2)
	for i := 0; i < iterations; i++ {
		responses = append(responses,
			chatTurnResponse("", "", []map[string]any{
				goalToolCall("bad", "unknown_tool", `{}`),
			}),
			chatTurnResponse("still working", "", nil),
		)
	}
	return responses
}

func goalCompletionResponses() []string {
	return []string{
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("complete", toolUpdateTodos, goalTodosArguments("work", "completed")),
			goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the final checklist is complete"]}`),
		}),
		chatTurnResponse("done", "", nil),
	}
}

func goalLastIncompleteTerminal(events []string) string {
	terminal := ""
	for _, event := range events {
		if strings.Contains(event, "[goal] incomplete") {
			terminal = event
		}
	}
	return terminal
}
