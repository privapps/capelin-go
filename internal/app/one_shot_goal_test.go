package app

import (
	"bytes"
	"capelin-go/internal/skills"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestOneShotGoalPersistenceFailureCannotBecomeSuccessAfterRecoverySave(t *testing.T) {
	completed := goalTodosArguments("work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("todo", toolUpdateTodos, completed),
			goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the final state was verified"]}`),
		}),
		chatTurnResponse("done", "", nil),
	)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.maxGoalIterations = 1
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	store.writeAtomic = func(path string, data []byte) error {
		if !failed && bytes.Contains(data, []byte(`"completion"`)) {
			failed = true
			return errors.New("transient final save failure")
		}
		return os.WriteFile(path, data, 0o600)
	}
	testApp.app.sessionStore = store

	err = testApp.app.runQuestion(context.Background(), "/goal verify the final state")
	if err == nil || !strings.Contains(err.Error(), "one-shot goal incomplete") {
		t.Fatalf("persistence failure was reported as success: %v", err)
	}
	if !failed {
		t.Fatal("persistence regression did not exercise the injected final-save failure")
	}
	snapshots, err := store.list()
	if err != nil || len(snapshots) != 1 || !validGoalCompletion(snapshots[0].ActiveGoal, snapshots[0].Todos) {
		t.Fatalf("recovery save did not preserve the latest durable completion: count=%d err=%v snapshots=%#v", len(snapshots), err, snapshots)
	}
}

func TestLeadingGoalCommandRecognizesOnlyTrimmedCommandBoundary(t *testing.T) {
	tests := []struct {
		input     string
		goal      bool
		objective string
	}{
		{input: "/goal", goal: true},
		{input: "  /goal\tfinish the work  ", goal: true, objective: "finish the work"},
		{input: "/goal\nfinish the work", goal: true, objective: "finish the work"},
		{input: "please /goal finish the work", goal: false},
		{input: "/goalist finish the work", goal: false},
		{input: "/goals finish the work", goal: false},
		{input: "/unknown finish the work", goal: false},
		{input: "goal /goal", goal: false},
	}
	for _, test := range tests {
		objective, got := leadingCommandArgument(test.input, "/goal")
		if got != test.goal || (got && objective != test.objective) {
			t.Fatalf("leading goal parse %q = (%q, %v), want (%q, %v)", test.input, objective, got, test.objective, test.goal)
		}
	}
}

func TestOneShotGoalRunsThroughDurableGoalWorkflowAndLoadsSkill(t *testing.T) {
	pending := goalTodosArguments("finish the work", "pending")
	completed := goalTodosArguments("finish the work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("seed", toolUpdateTodos, pending)}),
		chatTurnResponse("started", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("finish", toolUpdateTodos, completed)}),
		chatTurnResponse("verified", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the work was verified"]}`)}),
		chatTurnResponse("done", "", nil),
	)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.maxGoalIterations = 4
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	testApp.app.skills = map[string]skills.Skill{"implement": testSkill("implement", "implementation workflow guidance")}

	if err := testApp.app.runQuestion(context.Background(), "/goal $implement finish the work"); err != nil {
		t.Fatalf("one-shot goal failed: %v", err)
	}
	if got := len(testApp.userPrompts()); got != 6 {
		t.Fatalf("one-shot goal provider requests = %d, want 6", got)
	}
	if first := testApp.userPrompts()[0]; !strings.Contains(first, "implementation workflow guidance") || !strings.Contains(first, "finish the work") || !strings.Contains(first, "create_subagent") || !strings.Contains(first, "run_subagent using wait=false") {
		t.Fatalf("first goal turn did not include objective and selected skill: %q", first)
	}

	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.list()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("one-shot goal did not leave one durable session: count=%d err=%v", len(snapshots), err)
	}
	if !validGoalCompletion(snapshots[0].ActiveGoal, snapshots[0].Todos) {
		t.Fatalf("durable one-shot goal is not complete: goal=%#v todos=%#v", snapshots[0].ActiveGoal, snapshots[0].Todos)
	}
}

func TestOneShotGoalRejectsBeforeSessionOrProvider(t *testing.T) {
	for _, test := range []struct {
		name      string
		question  string
		yolo      bool
		wantError string
	}{
		{name: "missing objective", question: " /goal ", yolo: true, wantError: "one-shot goal requires an objective"},
		{name: "missing yolo", question: "/goal do the work", yolo: false, wantError: "--yolo is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testApp := newInteractiveTurnTestApp(t)
			testApp.app.cfg.yolo = test.yolo
			err := testApp.app.runQuestion(context.Background(), test.question)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("runQuestion error = %v, want %q", err, test.wantError)
			}
			if got := len(testApp.userPrompts()); got != 0 {
				t.Fatalf("rejected one-shot goal invoked provider: %#v", testApp.userPrompts())
			}
			if _, statErr := os.Stat(testApp.workspaceRoot + "/.capelin-go"); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected one-shot goal created session state: stat error=%v", statErr)
			}
		})
	}
}

func TestIncompleteOneShotGoalReturnsErrorAndPreservesResumeState(t *testing.T) {
	completed := goalTodosArguments("work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("complete", toolUpdateTodos, completed)}),
		chatTurnResponse("checked", "", nil),
	)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.maxGoalIterations = 1
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)

	err := testApp.app.runQuestion(context.Background(), "/goal leave a resumable incomplete goal")
	if err == nil || !strings.Contains(err.Error(), "one-shot goal incomplete") {
		t.Fatalf("incomplete one-shot goal error = %v", err)
	}
	store, storeErr := newSessionStore(testApp.workspaceRoot)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	snapshots, listErr := store.list()
	if listErr != nil || len(snapshots) != 1 {
		t.Fatalf("incomplete goal was not persisted: count=%d err=%v", len(snapshots), listErr)
	}
	if snapshots[0].ActiveGoal == nil || validGoalCompletion(snapshots[0].ActiveGoal, snapshots[0].Todos) {
		t.Fatalf("incomplete goal persisted invalid state: goal=%#v todos=%#v", snapshots[0].ActiveGoal, snapshots[0].Todos)
	}
}

func TestInteractiveInitialGoalUsesSharedGoalDispatch(t *testing.T) {
	pending := goalTodosArguments("finish the work", "pending")
	completed := goalTodosArguments("finish the work", "completed")
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{goalToolCall("seed", toolUpdateTodos, pending)}),
		chatTurnResponse("started", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("finish", toolUpdateTodos, completed)}),
		chatTurnResponse("verified", "", nil),
		chatTurnResponse("", "", []map[string]any{goalToolCall("claim", toolCompleteGoal, `{"summary":"finished","evidence":["the work was verified"]}`)}),
		chatTurnResponse("done", "", nil),
	)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.maxGoalIterations = 4
	testApp.app.cfg.allowedTools = map[string]bool{toolUpdateTodos: true}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	testApp.app.skills = map[string]skills.Skill{"implement": testSkill("implement", "implementation workflow guidance")}

	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runInteractiveInitialQuestion(context.Background(), session, "  /goal $implement finish the work  "); stopped {
		t.Fatal("interactive initial goal unexpectedly stopped the session")
	}
	if got := len(testApp.userPrompts()); got != 6 {
		t.Fatalf("interactive initial goal provider requests = %d, want 6", got)
	}
	if first := testApp.userPrompts()[0]; !strings.Contains(first, "implementation workflow guidance") || !strings.Contains(first, "finish the work") {
		t.Fatalf("initial goal turn did not include objective and selected skill: %q", first)
	}
	if !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("interactive initial goal is not complete: goal=%#v todos=%#v", session.activeGoal, session.todos)
	}
}

func TestInteractiveInitialNonGoalRemainsOrdinaryTurn(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, chatTurnResponse("ordinary response", "", nil))
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runInteractiveInitialQuestion(context.Background(), session, "  /goalist is ordinary text  "); stopped {
		t.Fatal("ordinary initial prompt unexpectedly stopped the session")
	}
	prompts := testApp.userPrompts()
	if len(prompts) != 1 || prompts[0] != "  /goalist is ordinary text  " {
		t.Fatalf("ordinary initial prompt dispatch = %#v, want one ordinary turn", prompts)
	}
	if session.activeGoal != nil {
		t.Fatalf("ordinary initial prompt created a goal: %#v", session.activeGoal)
	}
}
