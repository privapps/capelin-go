package app

import (
	"bytes"
	"capelin-go/internal/contracts"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIdleHookRunnerSerializesCallsAndDrains(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	active := 0
	maxActive := 0
	runner := newIdleHookRunner("hook", []string{"first", "second"}, t.TempDir(), false,
		func(_ context.Context, command, _ string, _ bool, args []string) (string, error) {
			if command != "hook" {
				t.Fatalf("command=%q", command)
			}
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			calls = append(calls, append([]string(nil), args...))
			mu.Unlock()
			mu.Lock()
			active--
			mu.Unlock()
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{})
	if runner == nil {
		t.Fatal("configured hook did not create a runner")
	}
	runner.trigger()
	runner.trigger()
	runner.drainAndClose()

	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("hook calls were concurrent: max active=%d", maxActive)
	}
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], []string{"first", "second"}) || !reflect.DeepEqual(calls[1], []string{"first", "second"}) {
		t.Fatalf("hook calls=%#v", calls)
	}
}

func TestOneShotTerminalOutcomeQueuesAndDrainsOneIdleHook(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer provider.Close()

	var calls int
	var gotCommand string
	var gotArgs []string
	runner := newIdleHookRunner("./hook", []string{"literal arg", "$(not shell)"}, t.TempDir(), false,
		func(_ context.Context, command, _ string, _ bool, args []string) (string, error) {
			calls++
			gotCommand = command
			gotArgs = append([]string(nil), args...)
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{})
	a := &app{
		cfg:       config{systemPrompt: "test", maxIterations: 2},
		client:    &client{endpoint: provider.URL + "/chat/completions", model: "test", http: provider.Client()},
		sink:      &turnEventSink{},
		idleHooks: runner,
	}
	if err := a.runQuestion(context.Background(), "finish"); err != nil {
		t.Fatalf("runQuestion: %v", err)
	}
	if calls != 1 {
		t.Fatalf("idle hook calls=%d, want 1", calls)
	}
	if gotCommand != "./hook" || !reflect.DeepEqual(gotArgs, []string{"literal arg", "$(not shell)"}) {
		t.Fatalf("hook invocation command=%q args=%#v", gotCommand, gotArgs)
	}
}

func TestIdleHookFailureDoesNotReplaceOneShotResult(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer provider.Close()

	var log bytes.Buffer
	runner := newIdleHookRunner("hook", nil, t.TempDir(), false,
		func(_ context.Context, _ string, _ string, _ bool, _ []string) (string, error) {
			return `{"exit_code":9,"failed":true,"stderr":"hook failed"}`, nil
		}, &log)
	a := &app{
		cfg:       config{systemPrompt: "test", maxIterations: 2},
		client:    &client{endpoint: provider.URL + "/chat/completions", model: "test", http: provider.Client()},
		sink:      &turnEventSink{},
		idleHooks: runner,
	}
	if err := a.runQuestion(context.Background(), "finish"); err != nil {
		t.Fatalf("hook failure replaced successful one-shot result: %v", err)
	}
	if !bytes.Contains(log.Bytes(), []byte("idle hook failed")) || !bytes.Contains(log.Bytes(), []byte("hook failed")) {
		t.Fatalf("missing bounded hook failure diagnostic: %q", log.String())
	}
}

var _ contracts.OutputSink = (*turnEventSink)(nil)

func TestBlankIdleHookIsDisabledWithoutStartingAProcess(t *testing.T) {
	called := false
	if runner := newIdleHookRunner("   ", []string{"ignored"}, t.TempDir(), false,
		func(context.Context, string, string, bool, []string) (string, error) {
			called = true
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{}); runner != nil {
		t.Fatal("blank hook command created a runner")
	}
	if called {
		t.Fatal("blank hook started an executor")
	}
}

func TestDefaultIdleHookExecutorUsesDirectProgramArguments(t *testing.T) {
	result, err := defaultIdleHookExecutor(context.Background(), "echo", t.TempDir(), false, []string{"literal arg", "$(printf shell)"})
	if err != nil {
		t.Fatalf("default idle hook executor: %v", err)
	}
	if !strings.Contains(result, `"exit_code": 0`) || !strings.Contains(result, "$(printf shell)") {
		t.Fatalf("direct-program result did not preserve exact arguments: %s", result)
	}
}

func TestInteractiveIdleHookRunsAfterCommitWithoutBlockingNextTurn(t *testing.T) {
	var mu sync.Mutex
	var events []string
	var calls int
	active := 0
	maxActive := 0
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})

	runner := newIdleHookRunner("hook", []string{"interactive"}, t.TempDir(), false,
		func(_ context.Context, _ string, _ string, _ bool, _ []string) (string, error) {
			mu.Lock()
			calls++
			call := calls
			active++
			if active > maxActive {
				maxActive = active
			}
			events = append(events, fmt.Sprintf("hook-%d", call))
			mu.Unlock()
			if call == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			mu.Lock()
			active--
			mu.Unlock()
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{})

	a := &app{idleHooks: runner}
	var idlePublished = make(chan struct{}, 2)
	a.interactiveIdleHook = func() {
		mu.Lock()
		events = append(events, "idle")
		mu.Unlock()
		idlePublished <- struct{}{}
	}
	session := &interactiveSession{}
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		a.finishInteractiveTurn(controller, session, outcome)
	}, a.triggerInteractiveIdleHook)

	if !a.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		worker.lastResponse = "first committed response"
		return false, nil
	}) {
		t.Fatal("first interactive turn did not start")
	}
	controller.wait()
	select {
	case <-idlePublished:
	case <-time.After(2 * time.Second):
		t.Fatal("first interactive turn did not publish idle")
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first interactive hook did not start")
	}

	// The first hook is still running, but the controller is idle and accepts
	// the next turn. Its hook must wait in the same serialized queue.
	if !a.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		worker.lastResponse = "second committed response"
		return false, nil
	}) {
		t.Fatal("second interactive turn was blocked by the running hook")
	}
	controller.wait()
	select {
	case <-idlePublished:
	case <-time.After(2 * time.Second):
		t.Fatal("second interactive turn did not publish idle")
	}
	close(releaseFirst)
	runner.drainAndClose()

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("interactive idle hook calls=%d, want 2", calls)
	}
	if maxActive != 1 {
		t.Fatalf("interactive hooks were concurrent: max active=%d", maxActive)
	}
	if len(events) != 4 || events[0] != "idle" || events[1] != "hook-1" || events[2] != "idle" || events[3] != "hook-2" {
		t.Fatalf("unexpected interactive lifecycle ordering: %v", events)
	}
	if session.lastResponse != "second committed response" {
		t.Fatalf("session was not finalized before the second turn: %q", session.lastResponse)
	}
}

func TestInteractiveIdleHookFailureDoesNotChangeTurnResult(t *testing.T) {
	var calls int
	var log bytes.Buffer
	runner := newIdleHookRunner("hook", nil, t.TempDir(), false,
		func(_ context.Context, _ string, _ string, _ bool, _ []string) (string, error) {
			calls++
			return `{"exit_code":7,"failed":true,"stderr":"background failure"}`, nil
		}, &log)
	a := &app{idleHooks: runner}
	session := &interactiveSession{lastResponse: "previous response"}
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		a.finishInteractiveTurn(controller, session, outcome)
	}, a.triggerInteractiveIdleHook)
	if !a.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		worker.lastResponse = "failed response must not commit"
		return false, errors.New("turn failed")
	}) {
		t.Fatal("failed interactive turn did not start")
	}
	controller.wait()
	runner.drainAndClose()
	if calls != 1 {
		t.Fatalf("failed interactive turn hook calls=%d, want 1", calls)
	}
	if session.lastResponse != "previous response" {
		t.Fatalf("failed turn changed session result: %q", session.lastResponse)
	}
	if !strings.Contains(log.String(), "idle hook failed") {
		t.Fatalf("hook failure was not isolated and logged: %q", log.String())
	}
}

func TestInteractiveInitialQuestionQueuesOneIdleHook(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	started := make(chan struct{})
	calls := 0
	runner := newIdleHookRunner("hook", []string{"initial"}, testApp.workspaceRoot, false,
		func(_ context.Context, _ string, _ string, _ bool, args []string) (string, error) {
			calls++
			if !reflect.DeepEqual(args, []string{"initial"}) {
				t.Errorf("initial hook args=%#v", args)
			}
			close(started)
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{})
	testApp.app.idleHooks = runner
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runInteractiveInitialQuestion(context.Background(), session, "initial question"); stopped {
		t.Fatal("successful initial question stopped the interactive session")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("initial question did not queue an idle hook")
	}
	runner.drainAndClose()
	if calls != 1 {
		t.Fatalf("initial idle hook calls=%d, want 1", calls)
	}
}
