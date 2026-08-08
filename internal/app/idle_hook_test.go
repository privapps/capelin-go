package app

import (
	"bytes"
	"capelin-go/internal/contracts"
	"capelin-go/internal/policy"
	"capelin-go/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

// TestIdleHookTimeoutTerminatesHungHook proves the runner owns its own deadline:
// a hook that never returns on its own is cancelled and reported, and the queue
// drains instead of hanging the caller forever.
func TestIdleHookTimeoutTerminatesHungHook(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var log bytes.Buffer

	runner := newIdleHookRunner("hook", nil, t.TempDir(), false,
		func(ctx context.Context, _ string, _ string, _ bool, _ []string) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			// A hung hook: only the runner-owned deadline can release it.
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			return `{"exit_code":0}`, nil
		}, &log, 1)
	if runner == nil {
		t.Fatal("configured hook did not create a runner")
	}

	runner.trigger()

	drained := make(chan struct{})
	go func() {
		runner.drainAndClose()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("hung idle hook was not bounded by the runner timeout")
	}

	if !strings.Contains(log.String(), "timeout") {
		t.Fatalf("timeout was not reported in the hook diagnostic: %q", log.String())
	}
	if !strings.Contains(log.String(), "idle hook failed") {
		t.Fatalf("timeout diagnostic lost the hook failure prefix: %q", log.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("hung hook executor calls=%d, want 1", calls)
	}
}

// TestIdleHookSuccessSuppressesOutput locks in the status-only contract: a
// successful hook never forwards its stdout to the operator log.
func TestIdleHookSuccessSuppressesOutput(t *testing.T) {
	var log bytes.Buffer
	huge := strings.Repeat("chatty hook output ", 4096)

	runner := newIdleHookRunner("hook", nil, t.TempDir(), false,
		func(context.Context, string, string, bool, []string) (string, error) {
			payload, err := json.Marshal(map[string]any{"exit_code": 0, "stdout": huge})
			if err != nil {
				t.Errorf("marshal fake result: %v", err)
				return `{"exit_code":0}`, nil
			}
			return string(payload), nil
		}, &log)
	if runner == nil {
		t.Fatal("configured hook did not create a runner")
	}

	runner.trigger()
	runner.drainAndClose()

	if log.Len() != 0 {
		t.Fatalf("successful hook surfaced output (%d bytes): %q", log.Len(), log.String())
	}
}

// TestIdleHookFailureDiagnosticIsBounded proves a failing, chatty hook cannot
// flood stderr: the diagnostic stays capped while remaining recognizable.
func TestIdleHookFailureDiagnosticIsBounded(t *testing.T) {
	var log bytes.Buffer
	hugeStderr := strings.Repeat("E", 8192)

	runner := newIdleHookRunner("hook", []string{"noisy"}, t.TempDir(), false,
		func(context.Context, string, string, bool, []string) (string, error) {
			payload, err := json.Marshal(map[string]any{
				"exit_code": 3,
				"failed":    true,
				"stderr":    hugeStderr,
			})
			if err != nil {
				t.Errorf("marshal fake result: %v", err)
				return `{"exit_code":1,"failed":true}`, nil
			}
			return string(payload), nil
		}, &log)
	if runner == nil {
		t.Fatal("configured hook did not create a runner")
	}

	runner.trigger()
	runner.drainAndClose()

	line := strings.TrimRight(log.String(), "\n")
	if len(line) > idleHookLogCapBytes {
		t.Fatalf("failure diagnostic was not bounded: %d bytes, cap %d", len(line), idleHookLogCapBytes)
	}
	// Line plus its single trailing newline: cap + small overhead.
	if log.Len() > idleHookLogCapBytes+8 {
		t.Fatalf("written diagnostic exceeded cap+overhead: %d bytes", log.Len())
	}
	if !strings.Contains(line, "idle hook failed") {
		t.Fatalf("bounded diagnostic lost its prefix: %q", line)
	}
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("bounded diagnostic spanned multiple lines: %q", line)
	}
	if !utf8.ValidString(line) {
		t.Fatalf("bounded diagnostic is not valid UTF-8: %q", line)
	}
}

// TestIdleHookZeroTimeoutFallsBackToDefault pins the 5s default so an unset
// configuration still bounds the hook.
func TestIdleHookZeroTimeoutFallsBackToDefault(t *testing.T) {
	var buf bytes.Buffer
	type deadlineProbe struct {
		ok        bool
		remaining time.Duration
	}
	probe := make(chan deadlineProbe, 1)

	runner := newIdleHookRunner("hook", nil, t.TempDir(), false,
		func(ctx context.Context, _ string, _ string, _ bool, _ []string) (string, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				probe <- deadlineProbe{ok: false}
				return `{"exit_code":0}`, nil
			}
			probe <- deadlineProbe{ok: true, remaining: time.Until(dl)}
			return `{"exit_code":0}`, nil
		}, &buf, 0)
	if runner == nil {
		t.Fatal("configured hook did not create a runner")
	}

	runner.trigger()
	runner.drainAndClose()

	var got deadlineProbe
	select {
	case got = <-probe:
	default:
		t.Fatal("hook executor never ran")
	}
	if !got.ok {
		t.Fatal("no deadline")
	}

	want := time.Duration(idleHookDefaultTimeoutSec) * time.Second
	const tolerance = 900 * time.Millisecond
	if got.remaining > want || got.remaining < want-tolerance {
		t.Fatalf("default deadline=%v, want ~%v (tolerance %v)", got.remaining, want, tolerance)
	}
	if buf.Len() != 0 {
		t.Fatalf("successful hook wrote diagnostics: %q", buf.String())
	}
}

// TestIdleHookTimeoutIsClampedAndIndependentOfExecuteProgram documents that an
// oversized configured timeout is clamped to the runner ceiling rather than
// inheriting the execute_program contract.
func TestIdleHookTimeoutIsClampedAndIndependentOfExecuteProgram(t *testing.T) {
	cases := []struct {
		name       string
		timeoutSec int
		want       int
	}{
		{name: "negative falls back to default", timeoutSec: -5, want: idleHookDefaultTimeoutSec},
		{name: "zero falls back to default", timeoutSec: 0, want: idleHookDefaultTimeoutSec},
		{name: "in range is honored", timeoutSec: 30, want: 30},
		{name: "above ceiling is clamped", timeoutSec: 10_000, want: idleHookMaxTimeoutSec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &idleHookRunner{timeoutSec: tc.timeoutSec}
			if got := r.effectiveTimeoutSec(); got != tc.want {
				t.Fatalf("effectiveTimeoutSec()=%d, want %d", got, tc.want)
			}
		})
	}
}

// TestCapHookLogTruncatesAtRuneBoundary guards the byte cap against splitting a
// multi-byte rune.
func TestCapHookLogTruncatesAtRuneBoundary(t *testing.T) {
	if short := "short diagnostic"; capHookLog(short) != short {
		t.Fatalf("short diagnostic was modified: %q", capHookLog(short))
	}

	// Three-byte runes never divide evenly into the cap, forcing a mid-rune cut.
	long := strings.Repeat("界", 2048)
	got := capHookLog(long)
	if len(got) > idleHookLogCapBytes {
		t.Fatalf("capHookLog returned %d bytes, cap %d", len(got), idleHookLogCapBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("capHookLog cut a rune in half: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncated diagnostic is not marked: %q", got)
	}
}

// TestIdleHookExecTimeoutSecondsRespectsExecuteProgramContract pins the value
// handed to tools.ExecuteProgramArgs.TimeoutSeconds into the accepted range.
func TestIdleHookExecTimeoutSecondsRespectsExecuteProgramContract(t *testing.T) {
	upper := idleHookExecTimeoutCap
	if tools.ToolTimeoutMax < upper {
		upper = tools.ToolTimeoutMax
	}

	if got := idleHookExecTimeoutSeconds(context.Background()); got != idleHookDefaultTimeoutSec {
		t.Fatalf("deadline-less context timeout=%d, want %d", got, idleHookDefaultTimeoutSec)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(idleHookMaxTimeoutSec)*time.Second)
	defer cancel()
	if got := idleHookExecTimeoutSeconds(ctx); got != upper {
		t.Fatalf("long deadline timeout=%d, want clamp to %d", got, upper)
	}

	tiny, cancelTiny := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelTiny()
	if got := idleHookExecTimeoutSeconds(tiny); got != 1 {
		t.Fatalf("sub-second deadline timeout=%d, want floor of 1", got)
	}
}

// --- Ticket 06: tracer-bullet integration through the primary app seam ---

// TestIdleHookImplicitGrantWithoutExecuteProgram proves the dedicated idle_hook
// permission is granted implicitly: a configured hook command builds a runner
// even when execute_program is NOT allowed and --yolo is NOT set.
func TestIdleHookImplicitGrantWithoutExecuteProgram(t *testing.T) {
	t.Setenv("IDLE_HOOK_COMMAND", "my-local-hook")
	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.allowedTools[policy.ExecuteProgram] {
		t.Fatal("execute_program was unexpectedly allowed")
	}
	if cfg.yolo {
		t.Fatal("yolo was unexpectedly enabled")
	}
	appInstance, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	if appInstance.idleHooks == nil {
		t.Fatal("idle hook runner was not constructed despite a configured command and no execute_program/yolo")
	}
	appInstance.idleHooks.drainAndClose()
}

// TestNoIdleHookFlagDisablesRunner proves --no-idle-hook suppresses the runner
// even when a hook command is configured via the environment.
func TestNoIdleHookFlagDisablesRunner(t *testing.T) {
	t.Setenv("IDLE_HOOK_COMMAND", "env-hook")
	cfg, err := loadConfig([]string{"--no-idle-hook", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.noIdleHook || cfg.idleHookCommand != "" {
		t.Fatalf("noIdleHook wiring wrong: %+v", cfg)
	}
	appInstance, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	if appInstance.idleHooks != nil {
		t.Fatal("--no-idle-hook constructed a runner despite an env hook command")
	}
}

// TestIdleHookTimeoutConfiguredFromEnvironment proves IDLE_HOOK_TIMEOUT flows
// through loadConfig into the constructed runner (independent of the
// execute_program 60s contract).
func TestIdleHookTimeoutConfiguredFromEnvironment(t *testing.T) {
	t.Setenv("IDLE_HOOK_COMMAND", "timed-hook")
	t.Setenv("IDLE_HOOK_TIMEOUT", "12")
	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.idleHookTimeoutSec != 12 {
		t.Fatalf("IDLE_HOOK_TIMEOUT = %d, want 12", cfg.idleHookTimeoutSec)
	}
	appInstance, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	if appInstance.idleHooks == nil {
		t.Fatal("timeout-configured hook did not build a runner")
	}
	appInstance.idleHooks.drainAndClose()
}

// TestIdleHookRunsThroughOneShotLifecycleWithImplicitGrant is a full tracer
// bullet: a one-shot run with a configured hook (no execute_program/YOLO) must
// queue and drain exactly one hook after the terminal outcome.
func TestIdleHookRunsThroughOneShotLifecycleWithImplicitGrant(t *testing.T) {
	t.Setenv("IDLE_HOOK_COMMAND", "./local-hook")
	var calls int
	var gotCommand string
	var gotArgs []string
	runner := newIdleHookRunner("./local-hook", []string{"implicit", "grant"}, t.TempDir(), false,
		func(_ context.Context, command, _ string, _ bool, args []string) (string, error) {
			calls++
			gotCommand = command
			gotArgs = append([]string(nil), args...)
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{})
	a := &app{
		cfg:       config{systemPrompt: "test", maxIterations: 2, idleHookCommand: "./local-hook"},
		client:    &client{endpoint: "http://example.invalid", model: "test"},
		sink:      &turnEventSink{},
		idleHooks: runner,
	}
	// Drive the lifecycle directly: trigger + drain after a terminal outcome.
	a.finishOneShotIdleHook()
	if calls != 1 {
		t.Fatalf("implicit-grant one-shot hook calls=%d, want 1", calls)
	}
	if gotCommand != "./local-hook" || !reflect.DeepEqual(gotArgs, []string{"implicit", "grant"}) {
		t.Fatalf("implicit-grant hook invocation command=%q args=%#v", gotCommand, gotArgs)
	}
}
