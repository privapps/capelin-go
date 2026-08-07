package app

import (
	"capelin-go/internal/contracts"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// overflowResponse returns a 400 response body that triggers the
// context-overflow detection in the provider adapter.
func overflowResponse() string {
	return `{"error":{"message":"invalid_request_body: This model's maximum context length is 32768 tokens","type":"invalid_request_body","code":400}}`
}

// goalOverflowTestSession creates a session seeded with a system message so that
// the overflow compaction path has messages to compact.
func goalOverflowTestSession(t *testing.T, testApp *interactiveTurnTestApp, seed bool) *interactiveSession {
	t.Helper()
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	session.messages = append(session.messages, contracts.Message{Role: "system", Content: "test system prompt for overflow recovery"})
	if seed {
		session.todos = []todoItem{{ID: "work", Content: "work", Status: todoStatusPending}}
		syncRuntimeTodos(session)
	}
	return session
}

// newOverflowTestApp creates a test app whose mock model server returns
// overflow responses for the first nRequests calls, then returns normal
// responses for the remainder. Normal responses are supplied as bodies.
// Compaction requests (containing the summarization prompt) are automatically
// detected and return a summary response without consuming a normal slot.
func newOverflowTestApp(t *testing.T, overflowCount int, normalBodies ...string) *interactiveTurnTestApp {
	t.Helper()
	testApp := &interactiveTurnTestApp{
		workspaceRoot: t.TempDir(),
		turnIdle:      make(chan struct{}, 16),
		responses:     normalBodies,
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if int(n) <= overflowCount {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(overflowResponse()))
			return
		}
		// Detect compaction requests by looking for the summarization prompt
		// in the request body. Compaction requests get a fixed summary without
		// consuming a normal response slot.
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			for _, m := range payload.Messages {
				if strings.Contains(m.Content, "Summarize the conversation below") {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"compacted summary of conversation"}}]}`))
					return
				}
			}
		}
		testApp.mu.Lock()
		idx := testApp.responseIndex
		if idx < len(testApp.responses) {
			testApp.responseIndex++
		}
		testApp.mu.Unlock()
		if idx < len(normalBodies) {
			_, _ = w.Write([]byte(normalBodies[idx]))
		} else {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		}
	}))
	t.Cleanup(server.Close)
	testApp.app = &app{
		cfg: config{
			model:           "test-model",
			maxIterations:   1,
			workspaceRoot:   testApp.workspaceRoot,
			toolMaxParallel: 1,
			toolTimeoutSec:  1,
			yolo:            true,
		},
		client: &client{
			endpoint: server.URL,
			model:    "test-model",
			http:     server.Client(),
		},
		sink: &spySink{},
	}
	testApp.app.interactiveIdleHook = func() { testApp.turnIdle <- struct{}{} }
	return testApp
}

// TestGoalOverflowRecoveryResetsBothStreaks proves that a goal iteration whose
// overflow recovery succeeded resets both the unchanged-checklist streak and
// the recovery streak.
func TestGoalOverflowRecoveryResetsBothStreaks(t *testing.T) {
	testApp := newOverflowTestApp(t, 1,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work", "completed")),
			goalToolCall("claim", toolCompleteGoal, `{"summary":"recovered and complete","evidence":["the final checklist is complete"]}`),
		}),
		chatTurnResponse("done", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true, toolCompleteGoal: true}, 1, 5)
	events := goalSystemEvents(testApp)
	session := goalOverflowTestSession(t, testApp, true)

	if stopped := testApp.app.runGoal(context.Background(), session, "recover overflow"); stopped {
		t.Fatal("overflow-recovery goal unexpectedly stopped the REPL")
	}
	if !validGoalCompletion(session.activeGoal, session.todos) {
		t.Fatalf("goal did not complete: todos=%#v goal=%#v", session.todos, session.activeGoal)
	}
	if containsEvent(*events, "stalled after") || containsEvent(*events, "recovery limit") {
		t.Fatalf("successful overflow recovery did not reset safeguards: %v", *events)
	}
}

// TestGoalOverflowRecoveryFailureAdvancesRecoveryStreak proves that a failed
// overflow recovery advances the recovery streak and that three consecutive
// failures end with the resumable outcome.
func TestGoalOverflowRecoveryFailureAdvancesRecoveryStreak(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t)
	// Server always returns overflow — compaction request also gets overflow.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(overflowResponse()))
	}))
	t.Cleanup(server.Close)
	testApp.app.client.endpoint = server.URL
	testApp.app.client.http = server.Client()

	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 10)
	events := goalSystemEvents(testApp)
	session := goalOverflowTestSession(t, testApp, true)

	_ = testApp.app.runGoal(context.Background(), session, "overflow fail")
	terminal := goalLastIncompleteTerminal(*events)
	if !strings.Contains(terminal, "recovery limit reached") {
		t.Fatalf("expected recovery limit, got: %q (events: %v)", terminal, *events)
	}
	if !strings.Contains(terminal, "overflow-recovery failure") {
		t.Fatalf("expected overflow-recovery failure message, got: %q", terminal)
	}
}

// TestGoalAutoCompactionCapExhaustionEndsResumably proves that when the
// automatic compaction cap is reached, the goal ends with the resumable
// outcome instead of a final failure.
func TestGoalAutoCompactionCapExhaustionEndsResumably(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Detect compaction requests by looking for the summarization prompt.
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			for _, m := range payload.Messages {
				if strings.Contains(m.Content, "Summarize the conversation below") {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"compacted summary"}}]}`))
					return
				}
			}
		}
		// Overflow every 3rd request to trigger recovery; retries succeed
		// with tool calls that update the checklist so the goal loop
		// continues and the auto-compaction cap is eventually hit.
		retryCount := int(n / 3)
		if n%3 == 1 {
			// Turn request: always overflow to trigger recovery
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(overflowResponse()))
		} else if retryCount <= 3 {
			// Compaction and retry succeed for the first 3 recovery cycles.
			// Retry responses include update_todos tool calls so the goal
			// loop sees progress and continues to the next iteration.
			resp := chatTurnResponse("", "", []map[string]any{
				goalToolCall("cap", toolUpdateTodos, goalTodosArguments("work", "in_progress")),
			})
			_, _ = w.Write([]byte(resp))
		} else {
			// After cap is hit, everything overflows (cap check prevents retry)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(overflowResponse()))
		}
	}))
	t.Cleanup(server.Close)
	testApp.app.client.endpoint = server.URL
	testApp.app.client.http = server.Client()

	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 20)
	events := goalSystemEvents(testApp)
	session := goalOverflowTestSession(t, testApp, true)

	_ = testApp.app.runGoal(context.Background(), session, "cap exhaustion")
	terminal := goalLastIncompleteTerminal(*events)
	if terminal == "" {
		t.Fatalf("no incomplete terminal outcome: %v", *events)
	}
	if strings.Contains(terminal, "provider or tool failure") {
		t.Fatalf("cap exhaustion ended as final failure: %q", terminal)
	}
	if !strings.Contains(terminal, "bare /goal") {
		t.Fatalf("cap exhaustion did not include resume guidance: %q", terminal)
	}
}

// TestOneShotGoalOverflowRecoverySucceeds proves that a one-shot /goal run
// with overflow that recovers and completes returns nil (success).
func TestOneShotGoalOverflowRecoverySucceeds(t *testing.T) {
	testApp := newOverflowTestApp(t, 1,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work", "completed")),
			goalToolCall("claim", toolCompleteGoal, `{"summary":"recovered and complete","evidence":["the final checklist is complete"]}`),
		}),
		chatTurnResponse("done", "", nil),
	)
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true, toolCompleteGoal: true}, 1, 5)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.finalOnly = true

	// runOneShotGoal creates a session with a system prompt internally.
	err := testApp.app.runOneShotGoal(context.Background(), "recover this task")
	if err != nil {
		t.Fatalf("one-shot overflow goal failed: %v", err)
	}
}

// TestOneShotGoalOverflowExhaustionReturnsIncomplete proves that a one-shot
// /goal run where overflow recovery is exhausted returns an incomplete error.
func TestOneShotGoalOverflowExhaustionReturnsIncomplete(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(overflowResponse()))
	}))
	t.Cleanup(server.Close)
	testApp.app.client.endpoint = server.URL
	testApp.app.client.http = server.Client()
	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true}, 1, 10)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.finalOnly = true

	err := testApp.app.runOneShotGoal(context.Background(), "exhaust this")
	if err == nil {
		t.Fatal("expected incomplete error for exhausted one-shot overflow goal")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected incomplete error, got: %v", err)
	}
}

// TestGoalOverflowRecoverySucceededSignalResetsStreaks proves that the
// overflowRecoverySucceeded flag correctly resets both streaks even when the
// checklist doesn't change and no tool activity occurs.
func TestGoalOverflowRecoverySucceededSignalResetsStreaks(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n%2 == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(overflowResponse()))
		} else {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"thinking..."}}]}`))
		}
	}))
	t.Cleanup(server.Close)
	testApp.app.client.endpoint = server.URL
	testApp.app.client.http = server.Client()
	testApp.responses = append(testApp.responses,
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("todo", toolUpdateTodos, goalTodosArguments("work", "completed")),
			goalToolCall("claim", toolCompleteGoal, `{"summary":"done","evidence":["the final checklist is complete"]}`),
		}),
		chatTurnResponse("finished", "", nil),
	)

	configureGoalTestApp(testApp, map[string]bool{toolUpdateTodos: true, toolCompleteGoal: true}, 1, 10)
	events := goalSystemEvents(testApp)
	session := goalOverflowTestSession(t, testApp, true)

	_ = testApp.app.runGoal(context.Background(), session, "streak reset via overflow")
	if containsEvent(*events, "stalled after") {
		t.Fatalf("stall guard fired despite overflow recovery: %v", *events)
	}
}

// --- Proactive context budget tests ---

// TestProactiveCompactionTriggeredWhenAboveThreshold proves that when the
// conversation size exceeds 75% of the configured context window, proactive
// compaction is triggered before the provider call.
func TestProactiveCompactionTriggeredWhenAboveThreshold(t *testing.T) {
	// Create a session with a large system message to exceed the threshold.
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("ok", "", nil),
	)
	testApp.app.cfg.contextWindow = 100 // very small budget for testing
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(msg string) {
		events = append(events, msg)
	}
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Seed with a large system message (> 75 chars so > 75 of 100 budget).
	bigContent := strings.Repeat("x", 100)
	session.messages = append(session.messages, contracts.Message{Role: "assistant", Content: bigContent})

	if stopped := testApp.app.runInteractiveTurn(context.Background(), session, "hello"); stopped {
		t.Fatal("proactive compaction turn unexpectedly stopped")
	}
	if !containsEvent(events, "proactive compaction") {
		t.Fatalf("proactive compaction was not triggered: %v", events)
	}
}

// TestProactiveCompactionSkippedWhenBelowThreshold proves that when the
// conversation is below the threshold, no proactive compaction occurs.
func TestProactiveCompactionSkippedWhenBelowThreshold(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("ok", "", nil),
	)
	testApp.app.cfg.contextWindow = 100000 // very large budget
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(msg string) {
		events = append(events, msg)
	}
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.runInteractiveTurn(context.Background(), session, "hello"); stopped {
		t.Fatal("below-threshold turn unexpectedly stopped")
	}
	if containsEvent(events, "proactive compaction") {
		t.Fatalf("proactive compaction should not trigger below threshold: %v", events)
	}
}

// TestNoProactiveCompactionWhenContextWindowUnset proves that when
// contextWindow is 0 (default), no proactive compaction occurs regardless of
// conversation size, preserving existing reactive-only behavior.
func TestNoProactiveCompactionWhenContextWindowUnset(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("ok", "", nil),
	)
	testApp.app.cfg.contextWindow = 0 // default/unset
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(msg string) {
		events = append(events, msg)
	}
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	bigContent := strings.Repeat("x", 50000)
	session.messages = append(session.messages, contracts.Message{Role: "assistant", Content: bigContent})

	if stopped := testApp.app.runInteractiveTurn(context.Background(), session, "hello"); stopped {
		t.Fatal("unset context window turn unexpectedly stopped")
	}
	if containsEvent(events, "proactive compaction") {
		t.Fatalf("proactive compaction should not trigger when contextWindow=0: %v", events)
	}
}

// TestProactiveCompactionFollowsOverflowSafeContract proves that proactive
// compaction retains the system prompt and produces a compacted-history marker.
func TestProactiveCompactionFollowsOverflowSafeContract(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		chatTurnResponse("ok", "", nil),
	)
	testApp.app.cfg.contextWindow = 100
	var events []string
	testApp.app.sink.(*spySink).onSystem = func(msg string) {
		events = append(events, msg)
	}
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Seed with a system message so proactive compaction has something to retain.
	session.messages = append(session.messages, contracts.Message{Role: "system", Content: "test system prompt"})
	bigContent := strings.Repeat("x", 100)
	session.messages = append(session.messages, contracts.Message{Role: "assistant", Content: bigContent})

	if stopped := testApp.app.runInteractiveTurn(context.Background(), session, "hello"); stopped {
		t.Fatal("proactive compaction contract turn unexpectedly stopped")
	}
	// Session should have been compacted: system message retained + compacted marker.
	foundMarker := false
	for _, msg := range session.messages {
		if msg.Role == "assistant" && strings.HasPrefix(msg.Content, compactedHistoryMarker) {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatalf("proactive compaction did not produce compacted-history marker: %#v", session.messages)
	}
	// System message should be preserved.
	if session.messages[0].Role != "system" {
		t.Fatalf("system message was not retained after proactive compaction: %#v", session.messages)
	}
}

// TestEstimateConversationSize validates the character estimation function.
func TestEstimateConversationSize(t *testing.T) {
	messages := []contracts.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	got := estimateConversationSize(messages, "follow up")
	want := len("You are helpful.") + len("hello") + len("hi there") + len("follow up")
	if got != want {
		t.Fatalf("estimateConversationSize = %d, want %d", got, want)
	}
}

// TestLoadConfigContextWindowFlag validates that --context-window is parsed
// correctly and invalid values are rejected.
func TestLoadConfigContextWindowFlag(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("CONTEXT_WINDOW", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.contextWindow != 0 {
		t.Fatalf("expected default contextWindow=0, got %d", cfg.contextWindow)
	}

	cfg, err = loadConfig([]string{"--context-window", "32000", "task"})
	if err != nil {
		t.Fatalf("loadConfig --context-window: %v", err)
	}
	if cfg.contextWindow != 32000 {
		t.Fatalf("expected contextWindow=32000, got %d", cfg.contextWindow)
	}

	cfg, err = loadConfig([]string{"--context-window=16000", "task"})
	if err != nil {
		t.Fatalf("loadConfig --context-window=: %v", err)
	}
	if cfg.contextWindow != 16000 {
		t.Fatalf("expected contextWindow=16000, got %d", cfg.contextWindow)
	}

	_, err = loadConfig([]string{"--context-window", "0", "task"})
	if err == nil {
		t.Fatal("expected error for --context-window=0")
	}
}

// TestLoadConfigContextWindowEnv validates that CONTEXT_WINDOW env var is
// parsed and CLI flag overrides it.
func TestLoadConfigContextWindowEnv(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("CONTEXT_WINDOW", "64000")
	defer t.Setenv("CONTEXT_WINDOW", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.contextWindow != 64000 {
		t.Fatalf("expected contextWindow=64000 from env, got %d", cfg.contextWindow)
	}

	// CLI flag wins over env.
	cfg, err = loadConfig([]string{"--context-window", "8000", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.contextWindow != 8000 {
		t.Fatalf("expected CLI flag (8000) to override env (64000), got %d", cfg.contextWindow)
	}
}
