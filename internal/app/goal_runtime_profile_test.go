package app

import (
	"capelin-go/internal/agent"
	configpkg "capelin-go/internal/config"
	"capelin-go/internal/contracts"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testRuntimeProfiles(ordinaryIterations, goalIterations, goalOuter, ordinaryParallel, goalParallel int) (configpkg.RuntimeProfile, configpkg.RuntimeProfile) {
	ordinary := configpkg.RuntimeProfile{
		MaxIterations:      ordinaryIterations,
		MaxGoalIterations:  goalOuter,
		ToolMaxParallel:    ordinaryParallel,
		ToolTimeoutSec:     1,
		ToolRetryOnTimeout: false,
	}
	goal := ordinary
	goal.MaxIterations = goalIterations
	goal.MaxGoalIterations = goalOuter
	goal.ToolMaxParallel = goalParallel
	goal.ToolTimeoutSec = 2
	return ordinary, goal
}

func TestAcceptedGoalProfileReachesRootLoopAndRestoresOrdinaryTurn(t *testing.T) {
	call := goalToolCall("inspect", toolListFiles, `{"path":"."}`)
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{call}),
		chatTurnResponse("", "", []map[string]any{call}),
		chatTurnResponse("goal safeguard", "", nil),
		chatTurnResponse("ordinary response", "", nil),
	)
	ordinary, goal := testRuntimeProfiles(1, 2, 1, 1, 2)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.allowedTools = map[string]bool{toolListFiles: true}
	testApp.app.cfg.ordinaryProfile = ordinary
	testApp.app.cfg.goalProfile = goal
	testApp.app.cfg.profilesResolved = true
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)

	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runGoal(context.Background(), session, "exercise the goal profile"); stopped {
		t.Fatal("goal safeguard unexpectedly stopped the interactive session")
	}
	// Two tool-bearing model responses plus the final-answer request prove the
	// goal profile's root limit of two was used. An ordinary limit of one would
	// make only two provider requests.
	if got := len(testApp.userPrompts()); got != 3 {
		t.Fatalf("goal root loop made %d provider requests, want 3", got)
	}
	if got := session.runtime.executionProfile; got != ordinary {
		t.Fatalf("goal profile leaked after safeguard: got=%+v ordinary=%+v", got, ordinary)
	}
	if session.runtime.maxToolIterations != ordinary.MaxIterations || session.runtime.goalIsEnabled() {
		t.Fatalf("goal runtime state was not restored: iterations=%d enabled=%v", session.runtime.maxToolIterations, session.runtime.goalIsEnabled())
	}

	if stopped := testApp.app.runInteractiveTurn(context.Background(), session, "ordinary follow-up"); stopped {
		t.Fatal("ordinary follow-up unexpectedly stopped the interactive session")
	}
	if got := len(testApp.userPrompts()); got != 4 {
		t.Fatalf("ordinary follow-up made %d provider requests, want 4", got)
	}
	if session.runtime.executionProfile != ordinary {
		t.Fatalf("ordinary follow-up did not retain ordinary profile: %+v", session.runtime.executionProfile)
	}
}

func TestGoalProfileReachesActualToolRunnerParallelism(t *testing.T) {
	var phase atomic.Int32
	ordinaryStarted := make(chan struct{}, 2)
	goalStarted := make(chan struct{}, 3)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 1 {
			ordinaryStarted <- struct{}{}
		} else {
			goalStarted <- struct{}{}
		}
		<-release
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	ordinary, goal := testRuntimeProfiles(1, 1, 1, 1, 2)
	a := &app{cfg: config{
		workspaceRoot:     t.TempDir(),
		allowedTools:      map[string]bool{toolFetchPage: true},
		allowPrivateFetch: true,
		ordinaryProfile:   ordinary,
		goalProfile:       goal,
		profilesResolved:  true,
	}, client: &client{http: server.Client()}, sink: &spySink{}}
	calls := []contracts.ToolCall{
		{ID: "one", Function: contracts.FunctionCall{Name: toolFetchPage, Arguments: `{"url":"` + server.URL + `"}`}},
		{ID: "two", Function: contracts.FunctionCall{Name: toolFetchPage, Arguments: `{"url":"` + server.URL + `"}`}},
	}

	phase.Store(1)
	ordinaryRuntime := a.rootRuntime()
	ordinaryDone := make(chan []agent.ToolResult, 1)
	ordinaryCapability := newAppToolCapability(nil, a, ordinaryRuntime)
	go func() { ordinaryDone <- ordinaryCapability.Run(context.Background(), calls) }()
	select {
	case <-ordinaryStarted:
	case <-time.After(time.Second):
		t.Fatal("ordinary tool runner did not start its first call")
	}
	select {
	case <-ordinaryStarted:
		t.Fatal("ordinary tool runner exceeded parallelism one")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-ordinaryDone:
	case <-time.After(time.Second):
		t.Fatal("ordinary tool runner did not finish")
	}

	release = make(chan struct{})
	phase.Store(2)
	goalRuntime := a.rootRuntime()
	restore := goalRuntime.selectExecutionProfile(goal)
	goalCapability := newAppToolCapability(nil, a, goalRuntime)
	goalDone := make(chan []agent.ToolResult, 1)
	go func() { goalDone <- goalCapability.Run(context.Background(), calls) }()
	for range 2 {
		select {
		case <-goalStarted:
		case <-time.After(time.Second):
			t.Fatal("goal tool runner did not reach configured parallelism")
		}
	}
	close(release)
	select {
	case <-goalDone:
	case <-time.After(time.Second):
		t.Fatal("goal tool runner did not finish")
	}
	restore()
	if goalRuntime.executionProfile != ordinary {
		t.Fatalf("tool-runner profile restoration failed: %+v", goalRuntime.executionProfile)
	}
}

func TestGoalCommandsPreserveYoloGateBeforeProviderOrStateChange(t *testing.T) {
	for _, input := range []string{"/goal objective", "/goal"} {
		t.Run(input, func(t *testing.T) {
			testApp := newInteractiveTurnTestApp(t)
			session, err := testApp.app.newInteractiveSession(nil)
			if err != nil {
				t.Fatal(err)
			}
			originalMessages := cloneMessages(session.messages)
			originalTodos := cloneTodos(session.todos)
			if stopped := testApp.app.handleInteractiveInput(context.Background(), session, input); stopped {
				t.Fatal("non-YOLO goal unexpectedly stopped the interactive session")
			}
			if len(testApp.userPrompts()) != 0 || len(session.messages) != len(originalMessages) || len(session.todos) != len(originalTodos) || session.activeGoal != nil {
				t.Fatalf("non-YOLO %q changed state or called provider: messages=%d todos=%#v goal=%#v requests=%#v", input, len(session.messages), session.todos, session.activeGoal, testApp.userPrompts())
			}
		})
	}
}

func TestResumedActiveGoalRecomputesProfileWithoutPersistedBudgetMode(t *testing.T) {
	call := goalToolCall("inspect", toolListFiles, `{"path":"."}`)
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("", "", []map[string]any{call}),
		chatTurnResponse("", "", []map[string]any{call}),
		chatTurnResponse("still working", "", nil),
	)
	ordinary, goal := testRuntimeProfiles(1, 2, 1, 1, 2)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.allowedTools = map[string]bool{toolListFiles: true}
	testApp.app.cfg.ordinaryProfile = ordinary
	testApp.app.cfg.goalProfile = goal
	testApp.app.cfg.profilesResolved = true
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)

	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.activeGoal = &goalState{Objective: "resume this objective", Generation: 7}
	session.todos = []todoItem{{ID: "work", Content: "unfinished work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatalf("save active goal: %v", err)
	}
	raw, err := os.ReadFile(testApp.app.sessionStore.pathFor(session.id))
	if err != nil {
		t.Fatalf("read saved active goal: %v", err)
	}
	if strings.Contains(string(raw), "budgetMode") || strings.Contains(string(raw), "budget_mode") {
		t.Fatalf("persisted active goal stored a budget mode: %s", raw)
	}

	resumedSnapshot, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatalf("resolve active goal: %v", err)
	}
	resumed := testApp.app.sessionFromSnapshot(resumedSnapshot)
	if resumed.runtime.executionProfile != ordinary {
		t.Fatalf("resumed session selected a non-ordinary profile before /goal: %+v", resumed.runtime.executionProfile)
	}
	if stopped := testApp.app.runGoal(context.Background(), resumed, ""); stopped {
		t.Fatal("resumed goal unexpectedly stopped the interactive session")
	}
	if got := len(testApp.userPrompts()); got != 3 {
		t.Fatalf("goal profile was not recomputed on resume: provider requests=%d, want 3", got)
	}
	if resumed.runtime.executionProfile != ordinary || resumed.runtime.maxToolIterations != ordinary.MaxIterations || resumed.runtime.goalIsEnabled() {
		t.Fatalf("resumed goal profile leaked after incomplete goal: runtime=%+v max=%d enabled=%v", resumed.runtime.executionProfile, resumed.runtime.maxToolIterations, resumed.runtime.goalIsEnabled())
	}
}
